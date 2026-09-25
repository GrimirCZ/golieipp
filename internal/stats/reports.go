package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// Query never migrates or creates a database. All dynamic SQL identifiers are
// selected from fixed report definitions, and all client filters are bound.
func Query(ctx context.Context, path string, o ReportOptions) (Report, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return Report{}, err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return Report{}, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return Report{}, err
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Report{}, err
	}
	defer tx.Rollback()
	var version int
	if err = tx.QueryRowContext(ctx, `SELECT version FROM stats_schema LIMIT 1`).Scan(&version); err != nil {
		return Report{}, fmt.Errorf("read statistics schema: %w", err)
	}
	if version != schemaVersion {
		return Report{}, fmt.Errorf("unsupported statistics schema version %d", version)
	}
	if o.Since.IsZero() {
		o.Since = time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -30)
	}
	if o.Until.IsZero() {
		o.Until = time.Now().UTC()
	}
	if !o.Since.Before(o.Until) {
		return Report{}, errors.New("since must precede until")
	}
	if o.Group != "" && o.Group != "day" && o.Group != "month" {
		return Report{}, errors.New("unsupported group")
	}
	if o.Limit <= 0 {
		o.Limit = 100
	}
	if o.Limit > 1000 {
		o.Limit = 1000
	}
	if o.Offset < 0 {
		return Report{}, errors.New("negative offset")
	}
	r := Report{Metadata: map[string]any{"requested_since": o.Since.UTC(), "requested_until": o.Until.UTC(), "unflushed_records_may_be_missing": true, "unknown_user": "", "nested_actions_overlap": true}}
	metaRows, _, err := readRows(ctx, tx, `SELECT * FROM stats_health`)
	if err != nil {
		return r, err
	}
	if len(metaRows) > 0 {
		r.Metadata["recording_health"] = metaRows[0]
		r.Metadata["known_recording_gaps"] = number(metaRows[0]["dropped"]) > 0 || number(metaRows[0]["errors"]) > 0
	}
	var encoded string
	if e := tx.QueryRowContext(ctx, `SELECT value FROM stats_meta WHERE key='options'`).Scan(&encoded); e == nil {
		var options map[string]any
		if json.Unmarshal([]byte(encoded), &options) == nil {
			r.Metadata["collection_options"] = options
			if n, ok := options["detail_retention_ns"].(float64); ok && n > 0 {
				cutoff := time.Now().UTC().Add(-time.Duration(n))
				r.Metadata["detail_retention_cutoff"] = cutoff
				r.Metadata["requested_detail_may_have_expired"] = o.Since.Before(cutoff)
			}
			if n, ok := options["cpu_retention_ns"].(float64); ok && n > 0 {
				cutoff := time.Now().UTC().Add(-time.Duration(n))
				r.Metadata["cpu_observation_retention_cutoff"] = cutoff
				r.Metadata["requested_cpu_observations_may_have_expired"] = o.Since.Before(cutoff)
			}
			if n, ok := options["rollup_retention_ns"].(float64); ok && n > 0 {
				cutoff := time.Now().UTC().Add(-time.Duration(n))
				r.Metadata["rollup_retention_cutoff"] = cutoff
				r.Metadata["requested_rollups_may_have_expired"] = o.Since.Before(cutoff)
			}
		}
	}
	var first sql.NullString
	_ = tx.QueryRowContext(ctx, `SELECT MIN(started_at) FROM runs`).Scan(&first)
	if first.Valid {
		r.Metadata["recording_started_at"] = first.String
		r.Metadata["requested_range_precedes_collection"] = formatTime(o.Since) < first.String
	}
	daily := o.Command == "summary" || o.Command == "users" || o.Command == "resources"
	since, until := formatTime(o.Since), formatTime(o.Until)
	if daily {
		start := o.Since.UTC().Truncate(24 * time.Hour)
		end := o.Until.UTC().Truncate(24 * time.Hour)
		if !end.Equal(o.Until) {
			end = end.AddDate(0, 0, 1)
		}
		since, until = dayString(start), dayString(end)
		r.Metadata["granularity"] = "UTC day"
		r.Metadata["effective_since"], r.Metadata["effective_until"] = since, until
	} else {
		r.Metadata["granularity"] = "exact timestamps"
	}
	where := func(column string) (string, []any) {
		q := column + ">=? AND " + column + "<?"
		a := []any{since, until}
		if o.Queue != "" {
			q += " AND queue=?"
			a = append(a, o.Queue)
		}
		if o.UserSet {
			q += " AND user_name=?"
			a = append(a, o.User)
		}
		return q, a
	}
	var query string
	var args []any
	switch o.Command {
	case "jobs":
		w, a := where("created_at")
		query = `SELECT *, (julianday(processing_at)-julianday(created_at))*86400 AS observed_processing_delay_seconds,(julianday(terminal_at)-julianday(created_at))*86400 AS observed_completion_seconds FROM stats_jobs WHERE ` + w + ` ORDER BY created_at DESC,registry_id,job_id LIMIT ? OFFSET ?`
		args = append(a, o.Limit, o.Offset)
	case "actions":
		w, a := where("finished_at")
		query = `SELECT * FROM stats_actions WHERE ` + w + ` ORDER BY finished_at DESC,id LIMIT ? OFFSET ?`
		args = append(a, o.Limit, o.Offset)
	case "summary", "users":
		w, a := where("submission_day")
		args = a
		var groups []string
		var fields []string
		if o.Group != "" {
			expression := "submission_day"
			if o.Group == "month" {
				expression = "substr(submission_day,1,7)"
			}
			groups = append(groups, expression)
			fields = append(fields, expression+" AS period")
		}
		if o.Command == "users" {
			groups = append(groups, "user_name")
			fields = append(fields, "user_name")
		}
		for _, key := range []string{"job_count", "accepted_count", "cancel_accepted_count", "completed_count", "canceled_count", "aborted_count", "failed_count", "rejected_count", "unresolved_count", "document_count", "copies", "payload_bytes"} {
			fields = append(fields, "COALESCE(SUM("+key+"),0) AS "+key)
		}
		for _, key := range []string{"page_count", "estimated_impressions", "completed_impressions", "completed_sheets"} {
			fields = append(fields, "CASE WHEN SUM("+key+"_known)>0 THEN SUM("+key+") END AS "+key, "COALESCE(SUM("+key+"_known),0) AS "+key+"_known")
		}
		for _, prefix := range []string{"processing", "completion"} {
			fields = append(fields, "SUM("+prefix+"_observations) AS "+prefix+"_observations", "SUM("+prefix+"_total_ns)*1.0/NULLIF(SUM("+prefix+"_observations),0) AS observed_"+prefix+"_average_ns", "MAX("+prefix+"_max_ns) AS observed_"+prefix+"_max_ns")
		}
		query = `SELECT ` + strings.Join(fields, ",") + ` FROM stats_job_rollups WHERE ` + w
		if len(groups) > 0 {
			query += ` GROUP BY ` + strings.Join(groups, ",") + ` ORDER BY ` + strings.Join(groups, ",")
		}
		settings, _, e := readRows(ctx, tx, `SELECT document_format,upstream_format,route,requested_color,effective_color,requested_sides,effective_sides,requested_media,effective_media,SUM(job_count) AS job_count FROM job_setting_rollups WHERE `+w+` GROUP BY document_format,upstream_format,route,requested_color,effective_color,requested_sides,effective_sides,requested_media,effective_media HAVING SUM(job_count)>0`, a...)
		if e != nil {
			return r, e
		}
		r.Metadata["print_settings"] = settings
		aw, aa := where("completion_day")
		actions, _, e := readRows(ctx, tx, actionSummarySQL+` FROM stats_action_rollups WHERE `+aw+` GROUP BY kind,operation,outcome ORDER BY kind,operation,outcome`, aa...)
		if e != nil {
			return r, e
		}
		r.Metadata["action_summary"] = actions
	case "resources":
		w, a := where("completion_day")
		components, _, e := readRows(ctx, tx, `SELECT kind,component,SUM(action_count) AS action_count,MAX(peak_capacity_bytes) AS peak_capacity_bytes FROM stats_buffer_rollups WHERE `+w+` GROUP BY kind,component ORDER BY kind,component`, a...)
		if e != nil {
			return r, e
		}
		r.Metadata["buffer_components"] = components
		query = actionSummarySQL + ` FROM stats_action_rollups WHERE ` + w + ` GROUP BY kind,operation,outcome ORDER BY kind,operation,outcome`
		args = a
		if o.UserSet || o.Queue != "" {
			r.Metadata["cpu_omitted"] = "process CPU cannot be attributed to a user or queue"
		} else {
			cpu, _, e := readRows(ctx, tx, `SELECT * FROM stats_cpu_daily WHERE day>=? AND day<? ORDER BY day`, since, until)
			if e != nil {
				return r, e
			}
			r.Metadata["cpu_daily"] = cpu
			runs, _, e := readRows(ctx, tx, `SELECT r.*,c.last_at AS cpu_last_observed_at,c.user_seconds AS cpu_last_user_seconds,c.system_seconds AS cpu_last_system_seconds FROM stats_runs r LEFT JOIN cpu_cursors c USING(run_id) WHERE r.started_at<? AND COALESCE(r.finished_at,?)>=? ORDER BY r.started_at`, formatTime(o.Until), formatTime(time.Now()), formatTime(o.Since))
			if e != nil {
				return r, e
			}
			r.Metadata["process_runs"] = runs
			intervals, _, e := readRows(ctx, tx, `WITH readings AS (SELECT *,lag(sampled_at) OVER (PARTITION BY run_id ORDER BY sampled_at) AS previous_at,lag(user_seconds+system_seconds) OVER (PARTITION BY run_id ORDER BY sampled_at) AS previous_cpu FROM cpu_samples WHERE available=1) SELECT run_id,sampled_at,previous_at,MAX(0,user_seconds+system_seconds-previous_cpu)/NULLIF((julianday(sampled_at)-julianday(previous_at))*86400,0)*100 AS cpu_percent FROM readings WHERE previous_at IS NOT NULL AND sampled_at>=? AND sampled_at<? ORDER BY sampled_at DESC LIMIT ? OFFSET ?`, formatTime(o.Since), formatTime(o.Until), o.Limit, o.Offset)
			if e != nil {
				return r, e
			}
			r.Metadata["cpu_intervals"] = intervals
			r.Metadata["cpu_attribution"] = "process-wide; daily deltas assigned to observation day; 100% is one core"
		}
	default:
		return r, fmt.Errorf("unknown statistics report %q", o.Command)
	}
	r.Rows, r.Columns, err = readRows(ctx, tx, query, args...)
	return r, err
}

const actionSummarySQL = `SELECT kind,operation,outcome,COALESCE(SUM(action_count),0) AS action_count,COALESCE(SUM(duration_ns),0) AS duration_ns,SUM(duration_ns)*1.0/NULLIF(SUM(action_count),0) AS average_duration_ns,MAX(duration_max_ns) AS max_duration_ns,SUM(client_bytes) AS client_bytes,SUM(upstream_bytes) AS upstream_bytes,SUM(output_bytes) AS output_bytes,MAX(buffer_peak_bytes) AS buffer_peak_bytes,MAX(temp_peak_bytes) AS temp_peak_bytes,SUM(temp_written_bytes) AS temp_written_bytes,MAX(concurrency_max) AS concurrency_max,MAX(user_concurrency_max) AS user_concurrency_max`

func readRows(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]map[string]any, []string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, err
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		row := make(map[string]any, len(columns))
		for i, k := range columns {
			if b, ok := values[i].([]byte); ok {
				row[k] = string(b)
			} else {
				row[k] = values[i]
			}
		}
		result = append(result, row)
	}
	return result, columns, rows.Err()
}
func number(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
