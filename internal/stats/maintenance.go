package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func (c *Collector) persistHealth(tx *sql.Tx) error {
	h := c.Health()
	_, err := tx.Exec(`UPDATE stats_health SET dropped=?,errors=?,last_persisted=?,last_error=? WHERE id=1`, h.Dropped, h.Errors, formatTime(time.Now()), h.LastError)
	return err
}

func (c *Collector) initializeMetadata() {
	var dropped, failures uint64
	var persisted sql.NullString
	var message string
	if err := c.db.QueryRow(`SELECT dropped,errors,last_persisted,last_error FROM stats_health WHERE id=1`).Scan(&dropped, &failures, &persisted, &message); err == nil {
		c.healthMu.Lock()
		c.health.Dropped += dropped
		c.health.Errors += failures
		c.health.LastError = message
		if persisted.Valid {
			c.health.LastPersisted, _ = time.Parse(time.RFC3339Nano, persisted.String)
		}
		c.healthMu.Unlock()
	}
	options, _ := json.Marshal(map[string]any{"detail_retention_ns": int64(c.opts.DetailRetention), "cpu_retention_ns": int64(c.opts.CPURetention), "rollup_retention_ns": int64(c.opts.RollupRetention), "resources": c.opts.Resources, "cpu": c.opts.CPU})
	_, err := c.db.Exec(`INSERT INTO stats_meta(key,value) VALUES('options',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(options))
	if err != nil {
		c.recordError(err)
	}
}

// Cleanup is called by the existing maintenance loop, not a resource sampler.
// A busy writer takes precedence; cleanup can run on the next maintenance pass.
func (c *Collector) Cleanup(now time.Time) {
	if c == nil || c.db == nil || c.closed.Load() || !c.writeMu.TryLock() {
		return
	}
	defer c.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		c.recordError(err)
		return
	}
	defer tx.Rollback()
	statements := []struct {
		query string
		arg   any
	}{}
	if c.opts.DetailRetention > 0 {
		cutoff := formatTime(now.Add(-c.opts.DetailRetention))
		statements = append(statements, struct {
			query string
			arg   any
		}{`DELETE FROM actions WHERE finished_at<?`, cutoff}, struct {
			query string
			arg   any
		}{`DELETE FROM jobs WHERE created_at<?`, cutoff}, struct {
			query string
			arg   any
		}{`DELETE FROM job_checkpoints WHERE unresolved=0 AND updated_at<?`, cutoff})
	}
	if c.opts.CPURetention > 0 {
		statements = append(statements, struct {
			query string
			arg   any
		}{`DELETE FROM cpu_samples WHERE sampled_at<?`, formatTime(now.Add(-c.opts.CPURetention))})
	}
	if c.opts.RollupRetention > 0 {
		for _, table := range []struct{ name, date string }{{"action_rollups", "completion_day"}, {"buffer_rollups", "completion_day"}, {"job_rollups", "submission_day"}, {"job_setting_rollups", "submission_day"}, {"cpu_daily", "day"}} {
			statements = append(statements, struct {
				query string
				arg   any
			}{`DELETE FROM ` + table.name + ` WHERE ` + table.date + `<?`, dayString(now.Add(-c.opts.RollupRetention))})
		}
	}
	for _, statement := range statements {
		if _, err = tx.ExecContext(ctx, statement.query, statement.arg); err != nil {
			break
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `DELETE FROM job_setting_rollups WHERE job_count=0`)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `INSERT INTO stats_meta(key,value) VALUES('last_cleanup',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, formatTime(now))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		c.recordError(err)
		c.warn("statistics retention cleanup failed", err)
	}
}
