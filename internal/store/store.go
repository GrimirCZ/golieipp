package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

type Job struct {
	ProxyJobID           int
	UpstreamJobID        int
	UpstreamJobURI       string
	Queue                string
	RequestingUser       string
	JobName              string
	DocumentFormat       string
	State                string
	PayloadBytes         int64
	PageCount            *int
	Copies               int
	EstimatedImpressions *int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS jobs (
	proxy_job_id INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_job_id INTEGER NOT NULL,
	upstream_job_uri TEXT NOT NULL DEFAULT '',
	queue TEXT NOT NULL,
	requesting_user TEXT NOT NULL DEFAULT '',
	job_name TEXT NOT NULL DEFAULT '',
	document_format TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT '',
	payload_bytes INTEGER NOT NULL DEFAULT 0,
	page_count INTEGER,
	copies INTEGER NOT NULL DEFAULT 1,
	estimated_impressions INTEGER,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_jobs_queue_upstream ON jobs(queue, upstream_job_id);
`)
	if err != nil {
		return err
	}
	return s.ensureColumns(ctx)
}

func (s *Store) CreateJob(ctx context.Context, job Job) (int, error) {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.Copies == 0 {
		job.Copies = 1
	}
	job.UpdatedAt = now
	res, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (upstream_job_id, upstream_job_uri, queue, requesting_user, job_name, document_format, state, payload_bytes, page_count, copies, estimated_impressions, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		job.UpstreamJobID, job.UpstreamJobURI, job.Queue, job.RequestingUser, job.JobName, job.DocumentFormat, job.State,
		job.PayloadBytes, nullableInt(job.PageCount), job.Copies, nullableInt(job.EstimatedImpressions),
		job.CreatedAt.Format(time.RFC3339Nano), job.UpdatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return int(id), err
}

func (s *Store) GetByProxyID(ctx context.Context, queue string, proxyJobID int) (Job, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT proxy_job_id, upstream_job_id, upstream_job_uri, queue, requesting_user, job_name, document_format, state,
payload_bytes, page_count, copies, estimated_impressions, created_at, updated_at
FROM jobs WHERE queue = ? AND proxy_job_id = ?`, queue, proxyJobID)
	return scanJob(row)
}

func (s *Store) GetByUpstreamID(ctx context.Context, queue string, upstreamJobID int) (Job, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT proxy_job_id, upstream_job_id, upstream_job_uri, queue, requesting_user, job_name, document_format, state,
payload_bytes, page_count, copies, estimated_impressions, created_at, updated_at
FROM jobs WHERE queue = ? AND upstream_job_id = ?`, queue, upstreamJobID)
	return scanJob(row)
}

func (s *Store) UpdateState(ctx context.Context, queue string, proxyJobID int, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = ? WHERE queue = ? AND proxy_job_id = ?`,
		state, time.Now().UTC().Format(time.RFC3339Nano), queue, proxyJobID)
	return err
}

func (s *Store) UpdatePayloadMetadata(ctx context.Context, queue string, proxyJobID int, documentFormat string, payloadBytes int64, pageCount *int, copies int, estimatedImpressions *int) error {
	if copies == 0 {
		copies = 1
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE jobs
SET document_format = CASE WHEN ? = '' THEN document_format ELSE ? END,
	payload_bytes = payload_bytes + ?,
	page_count = CASE
		WHEN ? IS NULL THEN page_count
		WHEN page_count IS NULL THEN ?
		ELSE page_count + ?
	END,
	copies = ?,
	estimated_impressions = CASE
		WHEN ? IS NULL THEN estimated_impressions
		WHEN estimated_impressions IS NULL THEN ?
		ELSE estimated_impressions + ?
	END,
	updated_at = ?
WHERE queue = ? AND proxy_job_id = ?`,
		documentFormat, documentFormat,
		payloadBytes,
		nullableInt(pageCount), nullableInt(pageCount), nullableInt(pageCount),
		copies,
		nullableInt(estimatedImpressions), nullableInt(estimatedImpressions), nullableInt(estimatedImpressions),
		time.Now().UTC().Format(time.RFC3339Nano), queue, proxyJobID)
	return err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var created, updated string
	var pageCount, impressions sql.NullInt64
	err := row.Scan(&job.ProxyJobID, &job.UpstreamJobID, &job.UpstreamJobURI, &job.Queue, &job.RequestingUser, &job.JobName,
		&job.DocumentFormat, &job.State, &job.PayloadBytes, &pageCount, &job.Copies, &impressions, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, err
	}
	if err != nil {
		return Job{}, err
	}
	if pageCount.Valid {
		v := int(pageCount.Int64)
		job.PageCount = &v
	}
	if impressions.Valid {
		v := int(impressions.Int64)
		job.EstimatedImpressions = &v
	}
	job.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return job, nil
}

func (s *Store) ensureColumns(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(jobs)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	columns := map[string]struct{}{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return err
		}
		columns[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	alter := []struct {
		name string
		sql  string
	}{
		{"upstream_job_uri", `ALTER TABLE jobs ADD COLUMN upstream_job_uri TEXT NOT NULL DEFAULT ''`},
		{"payload_bytes", `ALTER TABLE jobs ADD COLUMN payload_bytes INTEGER NOT NULL DEFAULT 0`},
		{"page_count", `ALTER TABLE jobs ADD COLUMN page_count INTEGER`},
		{"copies", `ALTER TABLE jobs ADD COLUMN copies INTEGER NOT NULL DEFAULT 1`},
		{"estimated_impressions", `ALTER TABLE jobs ADD COLUMN estimated_impressions INTEGER`},
	}
	for _, col := range alter {
		if _, ok := columns[col.name]; ok {
			continue
		}
		if _, err := s.db.ExecContext(ctx, col.sql); err != nil {
			return err
		}
	}
	return nil
}

func nullableInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}
