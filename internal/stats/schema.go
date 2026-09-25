package stats

import (
	"context"
	"database/sql"
	"fmt"
)

// schemaVersion is intentionally independent from the primary job registry.
// Statistics are a disposable accounting database, but a versioned schema
// lets a report process safely open a database produced by an older binary.
const schemaVersion = 1

const schemaSQL = `
CREATE TABLE IF NOT EXISTS stats_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS runs (
	run_id TEXT PRIMARY KEY,
	started_at TEXT NOT NULL,
	finished_at TEXT,
	cpu_start_available INTEGER NOT NULL DEFAULT 0,
	cpu_start_user_seconds REAL,
	cpu_start_system_seconds REAL,
	cpu_end_available INTEGER NOT NULL DEFAULT 0,
	cpu_end_user_seconds REAL,
	cpu_end_system_seconds REAL
);
CREATE TABLE IF NOT EXISTS actions (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	run_id TEXT NOT NULL,
	kind TEXT NOT NULL,
	operation TEXT NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	owner TEXT NOT NULL DEFAULT '',
	registry_id TEXT NOT NULL DEFAULT '',
	job_id INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL DEFAULT '',
	reason TEXT NOT NULL DEFAULT '',
	http_status INTEGER NOT NULL DEFAULT 0,
	ipp_status INTEGER NOT NULL DEFAULT 0,
	started_at TEXT NOT NULL,
	finished_at TEXT NOT NULL,
	duration_ns INTEGER NOT NULL DEFAULT 0,
	client_bytes INTEGER NOT NULL DEFAULT 0,
	upstream_bytes INTEGER NOT NULL DEFAULT 0,
	output_bytes INTEGER NOT NULL DEFAULT 0,
	buffer_peak_bytes INTEGER NOT NULL DEFAULT 0,
	buffer_components_json TEXT NOT NULL DEFAULT '',
	temp_peak_bytes INTEGER NOT NULL DEFAULT 0,
	temp_written_bytes INTEGER NOT NULL DEFAULT 0,
	concurrency INTEGER NOT NULL DEFAULT 0,
	user_concurrency INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_actions_finished ON actions(finished_at);
CREATE INDEX IF NOT EXISTS idx_actions_queue_user_finished ON actions(queue, user_name, finished_at);
CREATE INDEX IF NOT EXISTS idx_actions_run ON actions(run_id);
CREATE TABLE IF NOT EXISTS jobs (
	registry_id TEXT NOT NULL,
	job_id INTEGER NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	processing_at TEXT,
	terminal_at TEXT,
	submission_day TEXT NOT NULL,
	state TEXT NOT NULL DEFAULT '',
	observed_state TEXT NOT NULL DEFAULT '',
	state_reasons TEXT NOT NULL DEFAULT '',
	accepted INTEGER NOT NULL DEFAULT 0,
	cancel_accepted INTEGER NOT NULL DEFAULT 0,
	document_count INTEGER NOT NULL DEFAULT 0,
	copies INTEGER NOT NULL DEFAULT 0,
	payload_bytes INTEGER NOT NULL DEFAULT 0,
	page_count INTEGER,
	estimated_impressions INTEGER,
	completed_impressions INTEGER,
	completed_sheets INTEGER,
	document_format TEXT NOT NULL DEFAULT '',
	upstream_format TEXT NOT NULL DEFAULT '',
	route TEXT NOT NULL DEFAULT '',
	requested_color TEXT NOT NULL DEFAULT '',
	effective_color TEXT NOT NULL DEFAULT '',
	requested_sides TEXT NOT NULL DEFAULT '',
	effective_sides TEXT NOT NULL DEFAULT '',
	requested_media TEXT NOT NULL DEFAULT '',
	effective_media TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(registry_id, job_id)
);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_queue_user_created ON jobs(queue, user_name, created_at);
CREATE INDEX IF NOT EXISTS idx_jobs_terminal ON jobs(terminal_at);
CREATE TABLE IF NOT EXISTS action_rollups (
	completion_day TEXT NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	operation TEXT NOT NULL DEFAULT '',
	outcome TEXT NOT NULL DEFAULT '',
	action_count INTEGER NOT NULL DEFAULT 0,
	duration_ns INTEGER NOT NULL DEFAULT 0,
	duration_max_ns INTEGER NOT NULL DEFAULT 0,
	client_bytes INTEGER NOT NULL DEFAULT 0,
	upstream_bytes INTEGER NOT NULL DEFAULT 0,
	output_bytes INTEGER NOT NULL DEFAULT 0,
	buffer_peak_bytes INTEGER NOT NULL DEFAULT 0,
	temp_peak_bytes INTEGER NOT NULL DEFAULT 0,
	temp_written_bytes INTEGER NOT NULL DEFAULT 0,
	concurrency_max INTEGER NOT NULL DEFAULT 0,
	user_concurrency_max INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(completion_day, queue, user_name, kind, operation, outcome)
);
CREATE INDEX IF NOT EXISTS idx_action_rollups_day ON action_rollups(completion_day);
CREATE TABLE IF NOT EXISTS buffer_rollups (
 completion_day TEXT NOT NULL, queue TEXT NOT NULL, user_name TEXT NOT NULL,
 kind TEXT NOT NULL, component TEXT NOT NULL, action_count INTEGER NOT NULL,
 peak_capacity_bytes INTEGER NOT NULL,
 PRIMARY KEY(completion_day,queue,user_name,kind,component)
);
CREATE VIEW IF NOT EXISTS stats_buffer_rollups AS SELECT * FROM buffer_rollups;
CREATE TABLE IF NOT EXISTS job_rollups (
	submission_day TEXT NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	job_count INTEGER NOT NULL DEFAULT 0,
	accepted_count INTEGER NOT NULL DEFAULT 0,
	cancel_accepted_count INTEGER NOT NULL DEFAULT 0,
	unresolved_count INTEGER NOT NULL DEFAULT 0,
	completed_count INTEGER NOT NULL DEFAULT 0,
	canceled_count INTEGER NOT NULL DEFAULT 0,
	failed_count INTEGER NOT NULL DEFAULT 0,
	rejected_count INTEGER NOT NULL DEFAULT 0,
	aborted_count INTEGER NOT NULL DEFAULT 0,
	stopped_count INTEGER NOT NULL DEFAULT 0,
	document_count INTEGER NOT NULL DEFAULT 0,
	copies INTEGER NOT NULL DEFAULT 0,
	payload_bytes INTEGER NOT NULL DEFAULT 0,
	page_count INTEGER NOT NULL DEFAULT 0,
	page_count_known INTEGER NOT NULL DEFAULT 0,
	estimated_impressions INTEGER NOT NULL DEFAULT 0,
	estimated_impressions_known INTEGER NOT NULL DEFAULT 0,
	completed_impressions INTEGER NOT NULL DEFAULT 0,
	completed_impressions_known INTEGER NOT NULL DEFAULT 0,
	completed_sheets INTEGER NOT NULL DEFAULT 0,
	completed_sheets_known INTEGER NOT NULL DEFAULT 0,
 processing_observations INTEGER NOT NULL DEFAULT 0,
 processing_total_ns INTEGER NOT NULL DEFAULT 0,
 processing_max_ns INTEGER NOT NULL DEFAULT 0,
 completion_observations INTEGER NOT NULL DEFAULT 0,
 completion_total_ns INTEGER NOT NULL DEFAULT 0,
 completion_max_ns INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(submission_day, queue, user_name)
);
CREATE INDEX IF NOT EXISTS idx_job_rollups_day ON job_rollups(submission_day);
CREATE TABLE IF NOT EXISTS job_checkpoints (
	snapshot_json TEXT NOT NULL DEFAULT '{}',
	registry_id TEXT NOT NULL,
	job_id INTEGER NOT NULL,
	submission_day TEXT NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	accepted INTEGER NOT NULL DEFAULT 0,
	cancel_accepted INTEGER NOT NULL DEFAULT 0,
	unresolved INTEGER NOT NULL DEFAULT 1,
	document_count INTEGER NOT NULL DEFAULT 0,
	copies INTEGER NOT NULL DEFAULT 0,
	payload_bytes INTEGER NOT NULL DEFAULT 0,
	page_count INTEGER,
	estimated_impressions INTEGER,
	completed_impressions INTEGER,
	completed_sheets INTEGER,
	outcome TEXT NOT NULL DEFAULT '',
	document_format TEXT NOT NULL DEFAULT '',
	upstream_format TEXT NOT NULL DEFAULT '',
	route TEXT NOT NULL DEFAULT '',
	requested_color TEXT NOT NULL DEFAULT '',
	effective_color TEXT NOT NULL DEFAULT '',
	requested_sides TEXT NOT NULL DEFAULT '',
	effective_sides TEXT NOT NULL DEFAULT '',
	requested_media TEXT NOT NULL DEFAULT '',
	effective_media TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL,
	PRIMARY KEY(registry_id, job_id)
);
CREATE INDEX IF NOT EXISTS idx_job_checkpoints_day ON job_checkpoints(submission_day);
CREATE TABLE IF NOT EXISTS job_setting_rollups (
	submission_day TEXT NOT NULL,
	queue TEXT NOT NULL DEFAULT '',
	user_name TEXT NOT NULL DEFAULT '',
	document_format TEXT NOT NULL DEFAULT '',
	upstream_format TEXT NOT NULL DEFAULT '',
	route TEXT NOT NULL DEFAULT '',
	requested_color TEXT NOT NULL DEFAULT '',
	effective_color TEXT NOT NULL DEFAULT '',
	requested_sides TEXT NOT NULL DEFAULT '',
	effective_sides TEXT NOT NULL DEFAULT '',
	requested_media TEXT NOT NULL DEFAULT '',
	effective_media TEXT NOT NULL DEFAULT '',
	job_count INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(submission_day, queue, user_name, document_format, upstream_format, route,
		requested_color, effective_color, requested_sides, effective_sides,
		requested_media, effective_media)
);
CREATE INDEX IF NOT EXISTS idx_job_setting_rollups_day ON job_setting_rollups(submission_day);
CREATE TABLE IF NOT EXISTS cpu_samples (
	run_id TEXT NOT NULL,
	sampled_at TEXT NOT NULL,
	available INTEGER NOT NULL DEFAULT 0,
	user_seconds REAL NOT NULL DEFAULT 0,
	system_seconds REAL NOT NULL DEFAULT 0,
	PRIMARY KEY(run_id, sampled_at)
);
CREATE INDEX IF NOT EXISTS idx_cpu_samples_at ON cpu_samples(sampled_at);
CREATE TABLE IF NOT EXISTS cpu_cursors (
	run_id TEXT PRIMARY KEY,
	last_at TEXT,
	available INTEGER NOT NULL DEFAULT 0,
	user_seconds REAL NOT NULL DEFAULT 0,
	system_seconds REAL NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS cpu_daily (
	day TEXT PRIMARY KEY,
	user_seconds REAL NOT NULL DEFAULT 0,
	system_seconds REAL NOT NULL DEFAULT 0,
	sample_count INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS stats_health (
	id INTEGER PRIMARY KEY CHECK(id = 1),
	dropped INTEGER NOT NULL DEFAULT 0,
	errors INTEGER NOT NULL DEFAULT 0,
	last_persisted TEXT,
	last_error TEXT NOT NULL DEFAULT ''
);
INSERT INTO stats_health(id) VALUES(1) ON CONFLICT(id) DO NOTHING;

-- Stable read-only report surfaces. The underlying tables are intentionally
-- still available for embedders that need a lower-level export.
CREATE VIEW IF NOT EXISTS stats_actions AS SELECT * FROM actions;
CREATE VIEW IF NOT EXISTS stats_jobs AS SELECT * FROM jobs;
CREATE VIEW IF NOT EXISTS stats_action_rollups AS SELECT * FROM action_rollups;
CREATE VIEW IF NOT EXISTS stats_job_rollups AS SELECT * FROM job_rollups;
CREATE VIEW IF NOT EXISTS stats_cpu_daily AS SELECT * FROM cpu_daily;
CREATE VIEW IF NOT EXISTS stats_runs AS SELECT * FROM runs;
CREATE VIEW IF NOT EXISTS v_actions AS SELECT * FROM actions;
CREATE VIEW IF NOT EXISTS v_jobs AS SELECT * FROM jobs;
CREATE VIEW IF NOT EXISTS v_action_daily AS SELECT * FROM action_rollups;
CREATE VIEW IF NOT EXISTS v_job_daily AS SELECT * FROM job_rollups;
CREATE VIEW IF NOT EXISTS v_cpu_daily AS SELECT * FROM cpu_daily;
CREATE VIEW IF NOT EXISTS v_runs AS SELECT * FROM runs;
`

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin statistics schema migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS stats_schema (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("create statistics schema marker: %w", err)
	}
	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM stats_schema LIMIT 1`).Scan(&version)
	if err == sql.ErrNoRows {
		version = 0
	} else if err != nil {
		return fmt.Errorf("read statistics schema marker: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("statistics schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
			return fmt.Errorf("create statistics schema: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO stats_schema(version) VALUES(?)`, schemaVersion); err != nil {
			return fmt.Errorf("write statistics schema marker: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit statistics schema migration: %w", err)
	}
	return nil
}
