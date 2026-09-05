package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Lifecycle states used by the durable registry. The registry deliberately
// keeps lifecycle state separate from ObservedState: a request can be
// uncertain even when the last state observed from the printer was known.
const (
	StateReserved  = "reserved"
	StateMapped    = "mapped"
	StateUncertain = "uncertain"
	StateTerminal  = "terminal"

	// These terminal IPP states are also accepted in State for compatibility
	// with the original store, which stored the observed state directly there.
	StateCompleted = "completed"
	StateCanceled  = "canceled"
	StateCancelled = "cancelled"
	StateAborted   = "aborted"
	StateFailed    = "failed"
	StateStopped   = "stopped"
)

// JobState* aliases make the state vocabulary convenient to callers while
// retaining the shorter names used by the first store implementation.
const (
	JobStateReserved  = StateReserved
	JobStateMapped    = StateMapped
	JobStateUncertain = StateUncertain
	JobStateTerminal  = StateTerminal
	JobStateCompleted = StateCompleted
	JobStateCanceled  = StateCanceled
	JobStateCancelled = StateCancelled
	JobStateAborted   = StateAborted
	JobStateFailed    = StateFailed
	JobStateStopped   = StateStopped
)

const (
	// DefaultRetention is the default terminal-job retention period. Callers
	// that need a different policy should pass an explicit cutoff to cleanup.
	DefaultRetention    = 30 * 24 * time.Hour
	DefaultJobRetention = DefaultRetention

	// Reconciliation retries are deliberately paced. The initial delay keeps a
	// transiently uncertain job from being hammered, while the cap prevents a
	// permanently unmatched prefix of the candidate page from starving newer
	// uncertain jobs forever.
	reconciliationInitialBackoff = time.Minute
	reconciliationMaxBackoff     = time.Hour
)

var (
	// ErrMappingConflict indicates that an upstream job is already mapped to a
	// different proxy job in the same queue, or that an existing mapping is
	// being changed to a different upstream identity.
	ErrMappingConflict = errors.New("upstream job is already mapped")
	// ErrInvalidTransition indicates that a terminal registry row cannot be
	// moved back into an active lifecycle state.
	ErrInvalidTransition = errors.New("invalid job state transition")
	ErrInvalidArgument   = errors.New("invalid job registry argument")
)

// IsTerminalState reports whether state represents a terminal job. The
// generic "terminal" value is supported for callers that do not have a more
// specific IPP state.
func IsTerminalState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case StateTerminal, StateCompleted, StateCanceled, StateCancelled, StateAborted, StateFailed, StateStopped:
		return true
	default:
		return false
	}
}

type Store struct {
	db *sql.DB
}

// Job is the durable proxy-side representation of an upstream job.
//
// UpstreamJobID and UpstreamJobURI retain their original value types for
// source compatibility with the proxy. HasUpstreamJobID and
// HasUpstreamJobURI carry nullability explicitly: a reserved job has neither
// upstream identity yet, while a mapped job has one or both values.
type Job struct {
	ProxyJobID int

	UpstreamJobID        int
	HasUpstreamJobID     bool
	UpstreamJobURI       string
	HasUpstreamJobURI    bool
	Queue                string
	QueueOwner           string
	RequestingUser       string
	JobName              string
	DocumentFormat       string
	State                string
	ObservedState        string
	ObservedError        string
	PayloadBytes         int64
	PageCount            *int
	Copies               int
	EstimatedImpressions *int
	DocumentCount        int
	LastDocument         bool
	CreatedAt            time.Time
	UpdatedAt            time.Time
	TerminalAt           *time.Time
	ReconcileAt          *time.Time
	LastReconcileAt      *time.Time
	NextReconcileAt      *time.Time
}

// JobFilter describes the server-side filters needed by Get-Jobs and
// Cancel-My-Jobs in addition to administrative listing and reconciliation.
// Zero values mean no restriction.
type JobFilter struct {
	Queue          string
	QueueOwner     string
	RequestingUser string
	// User and RequestingUserName are compatibility aliases for callers that
	// use IPP terminology rather than the original store field name.
	User               string
	RequestingUserName string
	State              string
	States             []string
	ObservedState      string
	ProxyJobID         int
	UpstreamJobID      int
	CreatedAfter       time.Time
	CreatedBefore      time.Time
	UpdatedAfter       time.Time
	UpdatedBefore      time.Time
	TerminalBefore     time.Time
	TerminalAfter      time.Time
	IncludeTerminal    bool
	ExcludeTerminal    bool
	Limit              int
	Offset             int
}

// Reservation and ReserveRequest are aliases for Job so callers can make the
// intent of a pre-upstream row explicit without introducing a second model.
type Reservation = Job
type ReserveRequest = Job

// Open opens a SQLite-backed registry and applies all schema migrations in a
// single transaction. The connection pool remains single-connection because
// SQLite in-memory databases are connection-local and registry transitions
// must be serialized.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

const createJobsTableSQL = `
CREATE TABLE IF NOT EXISTS jobs (
	proxy_job_id INTEGER PRIMARY KEY AUTOINCREMENT,
	upstream_job_id INTEGER,
	upstream_job_uri TEXT,
	queue TEXT NOT NULL,
	queue_owner TEXT NOT NULL DEFAULT '',
	requesting_user TEXT NOT NULL DEFAULT '',
	job_name TEXT NOT NULL DEFAULT '',
	document_format TEXT NOT NULL DEFAULT '',
	state TEXT NOT NULL DEFAULT '',
	observed_state TEXT NOT NULL DEFAULT '',
	observed_error TEXT NOT NULL DEFAULT '',
	payload_bytes INTEGER NOT NULL DEFAULT 0,
	page_count INTEGER,
	copies INTEGER NOT NULL DEFAULT 1,
	estimated_impressions INTEGER,
	document_count INTEGER NOT NULL DEFAULT 0,
	last_document INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	terminal_at TEXT,
	reconcile_at TEXT,
	last_reconcile_at TEXT,
	next_reconcile_at TEXT
);`

var modernJobColumns = []struct {
	name string
	sql  string
}{
	{"upstream_job_id", `ALTER TABLE jobs ADD COLUMN upstream_job_id INTEGER`},
	{"upstream_job_uri", `ALTER TABLE jobs ADD COLUMN upstream_job_uri TEXT`},
	{"queue_owner", `ALTER TABLE jobs ADD COLUMN queue_owner TEXT NOT NULL DEFAULT ''`},
	{"requesting_user", `ALTER TABLE jobs ADD COLUMN requesting_user TEXT NOT NULL DEFAULT ''`},
	{"job_name", `ALTER TABLE jobs ADD COLUMN job_name TEXT NOT NULL DEFAULT ''`},
	{"document_format", `ALTER TABLE jobs ADD COLUMN document_format TEXT NOT NULL DEFAULT ''`},
	{"state", `ALTER TABLE jobs ADD COLUMN state TEXT NOT NULL DEFAULT ''`},
	{"observed_state", `ALTER TABLE jobs ADD COLUMN observed_state TEXT NOT NULL DEFAULT ''`},
	{"observed_error", `ALTER TABLE jobs ADD COLUMN observed_error TEXT NOT NULL DEFAULT ''`},
	{"payload_bytes", `ALTER TABLE jobs ADD COLUMN payload_bytes INTEGER NOT NULL DEFAULT 0`},
	{"page_count", `ALTER TABLE jobs ADD COLUMN page_count INTEGER`},
	{"copies", `ALTER TABLE jobs ADD COLUMN copies INTEGER NOT NULL DEFAULT 1`},
	{"estimated_impressions", `ALTER TABLE jobs ADD COLUMN estimated_impressions INTEGER`},
	{"document_count", `ALTER TABLE jobs ADD COLUMN document_count INTEGER NOT NULL DEFAULT 0`},
	{"last_document", `ALTER TABLE jobs ADD COLUMN last_document INTEGER NOT NULL DEFAULT 0`},
	{"created_at", `ALTER TABLE jobs ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`},
	{"updated_at", `ALTER TABLE jobs ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`},
	{"terminal_at", `ALTER TABLE jobs ADD COLUMN terminal_at TEXT`},
	{"reconcile_at", `ALTER TABLE jobs ADD COLUMN reconcile_at TEXT`},
	{"last_reconcile_at", `ALTER TABLE jobs ADD COLUMN last_reconcile_at TEXT`},
	{"next_reconcile_at", `ALTER TABLE jobs ADD COLUMN next_reconcile_at TEXT`},
}

// migrate upgrades the original schema without losing existing rows. SQLite
// cannot ALTER a NOT NULL column to nullable, so old tables are copied into a
// replacement table transactionally when that change is required.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var tableName string
	err = tx.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'jobs'`).Scan(&tableName)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, createJobsTableSQL); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	columns, err := tableInfo(ctx, tx)
	if err != nil {
		return err
	}
	// Add columns that can be added without a table rebuild first. This also
	// handles databases created by intermediate versions of the proxy.
	for _, col := range modernJobColumns {
		if _, ok := columns[col.name]; ok {
			continue
		}
		if _, err := tx.ExecContext(ctx, col.sql); err != nil {
			return err
		}
	}
	columns, err = tableInfo(ctx, tx)
	if err != nil {
		return err
	}

	if notNull(columns, "upstream_job_id") || notNull(columns, "upstream_job_uri") {
		if err := rebuildJobsTable(ctx, tx, columns); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET terminal_at = COALESCE(terminal_at, updated_at, created_at) WHERE terminal_at IS NULL AND lower(state) IN ('terminal','completed','canceled','cancelled','aborted','failed','stopped')`); err != nil {
		return fmt.Errorf("backfill terminal job timestamps: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_jobs_queue_state ON jobs(queue, state)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_jobs_queue_user ON jobs(queue, requesting_user)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_jobs_reconcile ON jobs(state, reconcile_at)`); err != nil {
		return err
	}
	// The old index was non-unique. Replacing it gives new mappings a durable
	// queue-scoped uniqueness guarantee. Refuse to open a legacy database with
	// duplicate mappings: silently weakening this invariant makes reconciliation
	// capable of attaching a client-visible Job to the wrong upstream Job.
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_jobs_queue_upstream`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX idx_jobs_queue_upstream ON jobs(queue, upstream_job_id) WHERE upstream_job_id IS NOT NULL`); err != nil {
		return fmt.Errorf("create unique upstream mapping index (repair duplicate queue/upstream job mappings before startup): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// ensureColumns remains as a compatibility shim for older package-internal
// callers. Open now performs the complete transactional migration.
func (s *Store) ensureColumns(_ context.Context) error { return nil }

type tableColumn struct {
	notNull bool
}

func tableInfo(ctx context.Context, tx *sql.Tx) (map[string]tableColumn, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(jobs)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columns := make(map[string]tableColumn)
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns[name] = tableColumn{notNull: notNull != 0}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return columns, nil
}

func notNull(columns map[string]tableColumn, name string) bool {
	column, ok := columns[name]
	return ok && column.notNull
}

var allJobColumnNames = []string{
	"proxy_job_id", "upstream_job_id", "upstream_job_uri", "queue", "queue_owner",
	"requesting_user", "job_name", "document_format", "state", "observed_state",
	"observed_error", "payload_bytes", "page_count", "copies", "estimated_impressions",
	"document_count", "last_document", "created_at", "updated_at", "terminal_at",
	"reconcile_at", "last_reconcile_at", "next_reconcile_at",
}

func rebuildJobsTable(ctx context.Context, tx *sql.Tx, oldColumns map[string]tableColumn) error {
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS jobs__migrating`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, strings.Replace(createJobsTableSQL, "jobs", "jobs__migrating", 1)); err != nil {
		return err
	}

	selectExpr := make([]string, 0, len(allJobColumnNames))
	for _, name := range allJobColumnNames {
		selectExpr = append(selectExpr, legacyColumnExpression(name, oldColumns))
	}
	query := fmt.Sprintf("INSERT INTO jobs__migrating (%s) SELECT %s FROM jobs", strings.Join(allJobColumnNames, ", "), strings.Join(selectExpr, ", "))
	if _, err := tx.ExecContext(ctx, query); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE jobs`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE jobs__migrating RENAME TO jobs`); err != nil {
		return err
	}
	return nil
}

func legacyColumnExpression(name string, columns map[string]tableColumn) string {
	if _, ok := columns[name]; ok {
		switch name {
		case "upstream_job_id":
			return `NULLIF(upstream_job_id, 0)`
		case "upstream_job_uri":
			return `NULLIF(upstream_job_uri, '')`
		case "document_count":
			return `COALESCE(document_count, 0)`
		case "last_document":
			return `COALESCE(last_document, 0)`
		default:
			return name
		}
	}
	switch name {
	case "proxy_job_id":
		return `rowid`
	case "upstream_job_id", "upstream_job_uri", "terminal_at", "reconcile_at", "last_reconcile_at", "next_reconcile_at":
		return `NULL`
	case "queue_owner", "requesting_user", "job_name", "document_format", "state", "observed_state", "observed_error":
		return `''`
	case "payload_bytes", "page_count", "estimated_impressions":
		return `0`
	case "copies":
		return `1`
	case "document_count":
		if _, hasPayload := columns["payload_bytes"]; hasPayload {
			return `CASE WHEN COALESCE(payload_bytes, 0) > 0 THEN 1 ELSE 0 END`
		}
		return `0`
	case "last_document":
		return `0`
	case "created_at", "updated_at":
		return `strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	default:
		return `NULL`
	}
}

// CreateJob is the compatibility API used by the original proxy. New code
// should reserve first and then mark the upstream mapping, but post-upstream
// creation remains valid for existing callers and tests.
func (s *Store) CreateJob(ctx context.Context, job Job) (int, error) {
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	if job.Copies == 0 {
		job.Copies = 1
	}
	if job.DocumentCount == 0 && job.PayloadBytes > 0 {
		job.DocumentCount = 1
	}
	if IsTerminalState(job.State) && job.TerminalAt == nil {
		terminalAt := job.UpdatedAt
		job.TerminalAt = &terminalAt
	}
	upstreamID := nullableUpstreamID(job)
	upstreamURI := nullableUpstreamURI(job)
	res, err := s.db.ExecContext(ctx, `
INSERT INTO jobs (upstream_job_id, upstream_job_uri, queue, queue_owner, requesting_user, job_name, document_format, state,
observed_state, observed_error, payload_bytes, page_count, copies, estimated_impressions, document_count, last_document,
created_at, updated_at, terminal_at, reconcile_at, last_reconcile_at, next_reconcile_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		upstreamID, upstreamURI, job.Queue, job.QueueOwner, job.RequestingUser, job.JobName, job.DocumentFormat, job.State,
		job.ObservedState, job.ObservedError, job.PayloadBytes, nullableInt(job.PageCount), job.Copies,
		nullableInt(job.EstimatedImpressions), job.DocumentCount, boolInt(job.LastDocument), formatTime(job.CreatedAt),
		formatTime(job.UpdatedAt), nullableTime(job.TerminalAt), nullableTime(job.ReconcileAt), nullableTime(job.LastReconcileAt),
		nullableTime(job.NextReconcileAt))
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	return int(id), err
}

// Reserve allocates a proxy job ID and durably records ownership before any
// upstream request is made. The returned ID is globally unique in the SQLite
// registry, while queue is retained on the row for scoped lookup and joins.

func (s *Store) Reserve(ctx context.Context, args ...any) (int, error) {
	job, err := parseReservationArgs(args)
	if err != nil {
		return 0, err
	}
	job.UpstreamJobID = 0
	job.HasUpstreamJobID = false
	job.UpstreamJobURI = ""
	job.HasUpstreamJobURI = false
	job.State = StateReserved
	job.ObservedState = ""
	job.ObservedError = ""
	job.PayloadBytes = 0
	job.PageCount = nil
	job.EstimatedImpressions = nil
	job.DocumentCount = 0
	job.LastDocument = false
	return s.CreateJob(ctx, job)
}

// ReserveJob is a descriptive alias for Reserve.
func (s *Store) ReserveJob(ctx context.Context, args ...any) (int, error) {
	return s.Reserve(ctx, args...)
}

// GetByProxyID accepts either (proxyJobID) or (queue, proxyJobID). The latter
// is retained for compatibility and scopes the lookup to a configured queue.
func (s *Store) GetByProxyID(ctx context.Context, args ...any) (Job, error) {
	queue, proxyJobID, err := parseScopedID(args)
	if err != nil {
		return Job{}, err
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs WHERE proxy_job_id = ?`
	queryArgs := []any{proxyJobID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	return scanJob(s.db.QueryRowContext(ctx, query, queryArgs...))
}

func (s *Store) GetJob(ctx context.Context, args ...any) (Job, error) {
	return s.GetByProxyID(ctx, args...)
}

func (s *Store) Lookup(ctx context.Context, args ...any) (Job, error) {
	return s.GetByProxyID(ctx, args...)
}

// GetByUpstreamID accepts either (upstreamJobID) or (queue, upstreamJobID).
func (s *Store) GetByUpstreamID(ctx context.Context, args ...any) (Job, error) {
	queue, upstreamJobID, err := parseScopedID(args)
	if err != nil {
		return Job{}, err
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs WHERE upstream_job_id = ?`
	queryArgs := []any{upstreamJobID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	return scanJob(s.db.QueryRowContext(ctx, query, queryArgs...))
}

func (s *Store) GetByUpstreamURI(ctx context.Context, args ...any) (Job, error) {
	queue, uri, err := parseScopedString(args)
	if err != nil {
		return Job{}, err
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs WHERE upstream_job_uri = ?`
	queryArgs := []any{uri}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	return scanJob(s.db.QueryRowContext(ctx, query, queryArgs...))
}

const jobSelectColumns = `proxy_job_id, upstream_job_id, upstream_job_uri, queue, queue_owner, requesting_user, job_name, document_format, state,
observed_state, observed_error, payload_bytes, page_count, copies, estimated_impressions, document_count, last_document,
created_at, updated_at, terminal_at, reconcile_at, last_reconcile_at, next_reconcile_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	var upstreamID sql.NullInt64
	var upstreamURI sql.NullString
	var pageCount, impressions sql.NullInt64
	var lastDocument int
	var created, updated, terminal, reconcile, lastReconcile, nextReconcile sql.NullString
	err := row.Scan(
		&job.ProxyJobID, &upstreamID, &upstreamURI, &job.Queue, &job.QueueOwner, &job.RequestingUser, &job.JobName,
		&job.DocumentFormat, &job.State, &job.ObservedState, &job.ObservedError, &job.PayloadBytes, &pageCount,
		&job.Copies, &impressions, &job.DocumentCount, &lastDocument, &created, &updated, &terminal, &reconcile,
		&lastReconcile, &nextReconcile)
	if err != nil {
		return Job{}, err
	}
	if upstreamID.Valid {
		job.HasUpstreamJobID = true
		job.UpstreamJobID = int(upstreamID.Int64)
	}
	if upstreamURI.Valid {
		job.HasUpstreamJobURI = true
		job.UpstreamJobURI = upstreamURI.String
	}
	if pageCount.Valid {
		value := int(pageCount.Int64)
		job.PageCount = &value
	}
	if impressions.Valid {
		value := int(impressions.Int64)
		job.EstimatedImpressions = &value
	}
	job.LastDocument = lastDocument != 0
	job.CreatedAt = parseTime(created)
	job.UpdatedAt = parseTime(updated)
	job.TerminalAt = parseNullableTime(terminal)
	job.ReconcileAt = parseNullableTime(reconcile)
	job.LastReconcileAt = parseNullableTime(lastReconcile)
	job.NextReconcileAt = parseNullableTime(nextReconcile)
	return job, nil
}

// MarkMapped records the upstream identity and makes a reservation usable for
// subsequent document/job operations. It accepts (proxyID, upstreamID, URI)
// or (queue, proxyID, upstreamID, URI) to keep queue-scoped callers concise.
func (s *Store) MarkMapped(ctx context.Context, args ...any) error {
	queue, proxyID, upstreamID, upstreamURI, err := parseMappingArgs(args)
	if err != nil {
		return err
	}
	return s.markMapped(ctx, queue, proxyID, upstreamID, upstreamURI)
}

func (s *Store) markMapped(ctx context.Context, queue string, proxyID, upstreamID int, upstreamURI string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	query := `SELECT state, upstream_job_id, upstream_job_uri FROM jobs WHERE proxy_job_id = ?`
	queryArgs := []any{proxyID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	var state string
	var currentID sql.NullInt64
	var currentURI sql.NullString
	if err := tx.QueryRowContext(ctx, query, queryArgs...).Scan(&state, &currentID, &currentURI); err != nil {
		return err
	}
	if IsTerminalState(state) {
		return ErrInvalidTransition
	}
	if currentID.Valid && upstreamID != 0 && int(currentID.Int64) != upstreamID {
		return ErrMappingConflict
	}
	if currentURI.Valid && upstreamURI != "" && currentURI.String != upstreamURI {
		return ErrMappingConflict
	}

	// A direct query gives callers a useful conflict error even on databases
	// opened from a legacy file where duplicate rows prevented the unique index.
	if upstreamID != 0 {
		// Always scope the duplicate check to the row's queue. When queue is
		// omitted by the caller, the proxy ID still tells us which queue owns
		// the mapping; otherwise identical upstream IDs on different printers
		// would be rejected.
		duplicateQuery := `SELECT proxy_job_id FROM jobs WHERE upstream_job_id = ? AND proxy_job_id <> ? AND queue = (SELECT queue FROM jobs WHERE proxy_job_id = ?)`
		duplicateArgs := []any{upstreamID, proxyID, proxyID}
		var duplicate int
		if err := tx.QueryRowContext(ctx, duplicateQuery, duplicateArgs...).Scan(&duplicate); err == nil {
			return ErrMappingConflict
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	now := time.Now().UTC()
	update := `UPDATE jobs SET upstream_job_id = ?, upstream_job_uri = ?, state = ?, observed_error = '', updated_at = ?, reconcile_at = NULL, next_reconcile_at = NULL WHERE proxy_job_id = ?`
	updateArgs := []any{nullableIntValue(upstreamID), nullableStringValue(upstreamURI), StateMapped, formatTime(now), proxyID}
	if queue != "" {
		update += ` AND queue = ?`
		updateArgs = append(updateArgs, queue)
	}
	result, err := tx.ExecContext(ctx, update, updateArgs...)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrMappingConflict
		}
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// MarkMappedNullable is useful to adapters that already represent upstream
// values as nullable pointers. A zero/nil value means no value is available.
func (s *Store) MarkMappedNullable(ctx context.Context, proxyID int, upstreamID *int, upstreamURI *string) error {
	var id int
	if upstreamID != nil {
		id = *upstreamID
	}
	var uri string
	if upstreamURI != nil {
		uri = *upstreamURI
	}
	return s.markMapped(ctx, "", proxyID, id, uri)
}

func (s *Store) MarkUncertain(ctx context.Context, args ...any) error {
	queue, proxyID, reason, at, err := parseUncertainArgs(args)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stateQuery := `SELECT state FROM jobs WHERE proxy_job_id = ?`
	stateArgs := []any{proxyID}
	if queue != "" {
		stateQuery += ` AND queue = ?`
		stateArgs = append(stateArgs, queue)
	}
	var currentState string
	if err := tx.QueryRowContext(ctx, stateQuery, stateArgs...).Scan(&currentState); err != nil {
		return err
	}
	if IsTerminalState(currentState) {
		return ErrInvalidTransition
	}
	query := `UPDATE jobs SET state = ?, observed_error = ?, updated_at = ?, reconcile_at = ?, next_reconcile_at = ? WHERE proxy_job_id = ?`
	queryArgs := []any{StateUncertain, reason, formatTime(at), formatTime(at), formatTime(at), proxyID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	result, err := tx.ExecContext(ctx, query, queryArgs...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// UpdateObserved records the most recent printer observation. It promotes a
// terminal observation through MarkTerminal; non-terminal observations do not
// silently resolve an uncertain lifecycle row because the reconciliation
// caller may still need to make a mapping decision.
func (s *Store) UpdateObserved(ctx context.Context, args ...any) error {
	queue, proxyID, observedState, observedError, at, err := parseObservedArgs(args)
	if err != nil {
		return err
	}
	if IsTerminalState(observedState) {
		terminalArgs := []any{proxyID, observedState, observedState, observedError}
		if queue != "" {
			terminalArgs = append([]any{queue}, terminalArgs...)
		}
		if !at.IsZero() {
			terminalArgs = append(terminalArgs, at)
		}
		return s.markTerminalParsed(ctx, terminalArgs)
	}
	query := `UPDATE jobs SET observed_state = ?, observed_error = ?, updated_at = ? WHERE proxy_job_id = ? AND terminal_at IS NULL AND lower(state) NOT IN ('terminal','completed','canceled','cancelled','aborted','failed','stopped')`
	queryArgs := []any{observedState, observedError, formatTime(timeOrNow(at)), proxyID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	result, err := s.db.ExecContext(ctx, query, queryArgs...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var terminalAt sql.NullString
		var currentState string
		lookupQuery := `SELECT terminal_at, state FROM jobs WHERE proxy_job_id = ?`
		lookupArgs := []any{proxyID}
		if queue != "" {
			lookupQuery += ` AND queue = ?`
			lookupArgs = append(lookupArgs, queue)
		}
		lookupErr := s.db.QueryRowContext(ctx, lookupQuery, lookupArgs...).Scan(&terminalAt, &currentState)
		if lookupErr != nil {
			return lookupErr
		}
		if terminalAt.Valid || IsTerminalState(currentState) {
			return ErrInvalidTransition
		}
		return sql.ErrNoRows
	}
	return nil
}

// MarkTerminal records a terminal lifecycle state. Supported forms are
// (proxyID, state, error), (proxyID, state, observedState, error), and the
// queue-prefixed equivalents.
func (s *Store) MarkTerminal(ctx context.Context, args ...any) error {
	return s.markTerminalParsed(ctx, args)
}

// MarkTerminalAt is the deterministic-clock variant used by retention jobs
// and tests. It accepts the same forms as MarkTerminal with a final time.Time.
func (s *Store) MarkTerminalAt(ctx context.Context, args ...any) error {
	return s.markTerminalParsed(ctx, args)
}

func (s *Store) markTerminalParsed(ctx context.Context, args []any) error {
	queue, proxyID, terminalState, observedState, observedError, at, err := parseTerminalArgs(args)
	if err != nil {
		return err
	}
	if observedState == "" && terminalState != "" && terminalState != StateTerminal {
		observedState = terminalState
	}
	// State is the durable registry lifecycle, while the printer's specific
	// terminal state is retained in ObservedState. This keeps the reserved ->
	// mapped -> uncertain -> terminal state machine independent of IPP's many
	// terminal spellings.
	terminalState = StateTerminal
	if at.IsZero() {
		at = time.Now().UTC()
	}
	query := `UPDATE jobs SET state = ?, observed_state = ?, observed_error = ?, terminal_at = COALESCE(terminal_at, ?), updated_at = ?, reconcile_at = NULL, next_reconcile_at = NULL WHERE proxy_job_id = ?`
	queryArgs := []any{terminalState, observedState, observedError, formatTime(at), formatTime(at), proxyID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	result, err := s.db.ExecContext(ctx, query, queryArgs...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateState is the original state mutation API. Terminal states gain a
// terminal timestamp automatically; all other values preserve compatibility.
func (s *Store) UpdateState(ctx context.Context, queue string, proxyJobID int, state string) error {
	now := time.Now().UTC()
	if IsTerminalState(state) {
		// Preserve the original API's direct state semantics (callers already
		// pass values such as "failed"), while the richer MarkTerminal API uses
		// the generic StateTerminal lifecycle plus ObservedState.
		result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, observed_state = CASE WHEN ? = '' THEN observed_state ELSE ? END, terminal_at = COALESCE(terminal_at, ?), updated_at = ?, reconcile_at = NULL, next_reconcile_at = NULL WHERE queue = ? AND proxy_job_id = ?`,
			state, state, state, formatTime(now), formatTime(now), queue, proxyJobID)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state = ?, updated_at = ? WHERE queue = ? AND proxy_job_id = ? AND terminal_at IS NULL AND lower(state) NOT IN ('terminal','completed','canceled','cancelled','aborted','failed','stopped')`,
		state, formatTime(now), queue, proxyJobID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		var terminalAt sql.NullString
		var currentState string
		lookupErr := s.db.QueryRowContext(ctx, `SELECT terminal_at, state FROM jobs WHERE queue = ? AND proxy_job_id = ?`, queue, proxyJobID).Scan(&terminalAt, &currentState)
		if lookupErr != nil {
			return lookupErr
		}
		if terminalAt.Valid || IsTerminalState(currentState) {
			return ErrInvalidTransition
		}
		return sql.ErrNoRows
	}
	return nil
}

// UpdateDocument records one document and its metadata. It accepts the
// queue-less form (proxyID, format, bytes, pages, copies, impressions,
// lastDocument) and a queue-prefixed form.
func (s *Store) UpdateDocument(ctx context.Context, args ...any) error {
	queue, proxyID, format, payloadBytes, pageCount, copies, impressions, lastDocument, err := parseDocumentArgs(args)
	if err != nil {
		return err
	}
	if copies == 0 {
		copies = 1
	}
	query := `
UPDATE jobs
SET document_format = CASE WHEN ? = '' THEN document_format ELSE ? END,
	payload_bytes = payload_bytes + ?,
	page_count = CASE WHEN ? IS NULL THEN page_count WHEN page_count IS NULL THEN ? ELSE page_count + ? END,
	copies = ?,
	estimated_impressions = CASE WHEN ? IS NULL THEN estimated_impressions WHEN estimated_impressions IS NULL THEN ? ELSE estimated_impressions + ? END,
	document_count = document_count + CASE WHEN ? > 0 THEN 1 ELSE 0 END,
	last_document = CASE WHEN ? <> 0 THEN 1 ELSE last_document END,
	updated_at = ?
WHERE proxy_job_id = ?`
	queryArgs := []any{format, format, payloadBytes, nullableInt(pageCount), nullableInt(pageCount), nullableInt(pageCount), copies,
		nullableInt(impressions), nullableInt(impressions), nullableInt(impressions), payloadBytes, boolInt(lastDocument), formatTime(time.Now().UTC()), proxyID}
	if queue != "" {
		query += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	result, err := s.db.ExecContext(ctx, query, queryArgs...)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdatePayloadMetadata retains the old API and records each call as one
// document, leaving LastDocument unchanged because the old signature had no
// last-document argument.
func (s *Store) UpdatePayloadMetadata(ctx context.Context, queue string, proxyJobID int, documentFormat string, payloadBytes int64, pageCount *int, copies int, estimatedImpressions *int) error {
	return s.UpdateDocument(ctx, queue, proxyJobID, documentFormat, payloadBytes, pageCount, copies, estimatedImpressions, false)
}

func (s *Store) UpdatePayloadMetadataWithLastDocument(ctx context.Context, queue string, proxyJobID int, documentFormat string, payloadBytes int64, pageCount *int, copies int, estimatedImpressions *int, lastDocument bool) error {
	return s.UpdateDocument(ctx, queue, proxyJobID, documentFormat, payloadBytes, pageCount, copies, estimatedImpressions, lastDocument)
}

// ListJobs returns rows in deterministic creation order and applies all
// supplied filters in SQL so queue/user ownership checks are not bypassed by
// callers that handle Get-Jobs or Cancel-My-Jobs.
func (s *Store) ListJobs(ctx context.Context, args ...any) ([]Job, error) {
	filter, err := parseJobFilterArgs(args)
	if err != nil {
		return nil, err
	}
	where := make([]string, 0, 8)
	queryArgs := make([]any, 0, 12)
	if filter.Queue != "" {
		where = append(where, "queue = ?")
		queryArgs = append(queryArgs, filter.Queue)
	}
	if filter.QueueOwner != "" {
		where = append(where, "queue_owner = ?")
		queryArgs = append(queryArgs, filter.QueueOwner)
	}
	user := filter.RequestingUser
	if user == "" {
		user = filter.User
	}
	if user == "" {
		user = filter.RequestingUserName
	}
	if user != "" {
		where = append(where, "requesting_user = ?")
		queryArgs = append(queryArgs, user)
	}
	if filter.State != "" {
		where = append(where, "state = ?")
		queryArgs = append(queryArgs, filter.State)
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			placeholders[i] = "?"
			queryArgs = append(queryArgs, state)
		}
		where = append(where, "state IN ("+strings.Join(placeholders, ",")+")")
	}
	if filter.ObservedState != "" {
		where = append(where, "observed_state = ?")
		queryArgs = append(queryArgs, filter.ObservedState)
	}
	if filter.ProxyJobID != 0 {
		where = append(where, "proxy_job_id = ?")
		queryArgs = append(queryArgs, filter.ProxyJobID)
	}
	if filter.UpstreamJobID != 0 {
		where = append(where, "upstream_job_id = ?")
		queryArgs = append(queryArgs, filter.UpstreamJobID)
	}
	if !filter.CreatedAfter.IsZero() {
		where = append(where, "created_at > ?")
		queryArgs = append(queryArgs, formatTime(filter.CreatedAfter))
	}
	if !filter.CreatedBefore.IsZero() {
		where = append(where, "created_at < ?")
		queryArgs = append(queryArgs, formatTime(filter.CreatedBefore))
	}
	if !filter.UpdatedAfter.IsZero() {
		where = append(where, "updated_at > ?")
		queryArgs = append(queryArgs, formatTime(filter.UpdatedAfter))
	}
	if !filter.UpdatedBefore.IsZero() {
		where = append(where, "updated_at < ?")
		queryArgs = append(queryArgs, formatTime(filter.UpdatedBefore))
	}
	if !filter.TerminalBefore.IsZero() {
		where = append(where, "terminal_at IS NOT NULL AND terminal_at < ?")
		queryArgs = append(queryArgs, formatTime(filter.TerminalBefore))
	}
	if !filter.TerminalAfter.IsZero() {
		where = append(where, "terminal_at IS NOT NULL AND terminal_at > ?")
		queryArgs = append(queryArgs, formatTime(filter.TerminalAfter))
	}
	if filter.IncludeTerminal && filter.ExcludeTerminal {
		return nil, ErrInvalidArgument
	}
	if filter.ExcludeTerminal {
		where = append(where, "terminal_at IS NULL")
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY created_at ASC, proxy_job_id ASC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		queryArgs = append(queryArgs, filter.Limit)
		if filter.Offset > 0 {
			query += ` OFFSET ?`
			queryArgs = append(queryArgs, filter.Offset)
		}
	} else if filter.Offset > 0 {
		query += ` LIMIT -1 OFFSET ?`
		queryArgs = append(queryArgs, filter.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) List(ctx context.Context, args ...any) ([]Job, error) {
	return s.ListJobs(ctx, args...)
}

// GetJobs accepts a JobFilter, a queue, or (queue, requestingUser), plus an
// optional filter. This shape maps directly to the IPP Get-Jobs variants.
func (s *Store) GetJobs(ctx context.Context, args ...any) ([]Job, error) {
	filter, err := parseJobFilterArgs(args)
	if err != nil {
		return nil, err
	}
	return s.ListJobs(ctx, filter)
}

func (s *Store) MyJobs(ctx context.Context, queue, requestingUser string) ([]Job, error) {
	return s.ListJobs(ctx, JobFilter{Queue: queue, RequestingUser: requestingUser})
}

func (s *Store) GetMyJobs(ctx context.Context, queue, requestingUser string) ([]Job, error) {
	return s.MyJobs(ctx, queue, requestingUser)
}

func (s *Store) ListMyJobs(ctx context.Context, queue, requestingUser string) ([]Job, error) {
	return s.MyJobs(ctx, queue, requestingUser)
}

// ReconciliationCandidates returns uncertain jobs whose next reconciliation
// time is due. Accepted arguments are (now, limit), (limit), or
// (queue, now, limit); all are optional.
func (s *Store) ReconciliationCandidates(ctx context.Context, args ...any) ([]Job, error) {
	queue, at, limit, err := parseReconcileArgs(args)
	if err != nil {
		return nil, err
	}
	where := `state = ? AND (next_reconcile_at IS NULL OR next_reconcile_at <= ?)`
	queryArgs := []any{StateUncertain, formatTime(at)}
	if queue != "" {
		where += ` AND queue = ?`
		queryArgs = append(queryArgs, queue)
	}
	query := `SELECT ` + jobSelectColumns + ` FROM jobs WHERE ` + where + ` ORDER BY COALESCE(next_reconcile_at, reconcile_at, updated_at), proxy_job_id`
	if limit > 0 {
		query += ` LIMIT ?`
		queryArgs = append(queryArgs, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := make([]Job, 0)
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) ListReconciliationCandidates(ctx context.Context, args ...any) ([]Job, error) {
	return s.ReconciliationCandidates(ctx, args...)
}

func (s *Store) MarkReconcileAttempt(ctx context.Context, args ...any) error {
	proxyID, at, err := parseIDTimeArgs(args)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	// Read and write the retry timestamps in one transaction. Reconciliation
	// runs from a bounded candidate page and may have more than one caller, so
	// deriving the next delay outside a transaction could lose an increment.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var previousReconcile, previousNext sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT reconcile_at, next_reconcile_at FROM jobs WHERE proxy_job_id = ?`, proxyID).
		Scan(&previousReconcile, &previousNext); err != nil {
		return err
	}
	backoff := reconciliationBackoff(parseTime(previousReconcile), parseTime(previousNext))
	next := at.Add(backoff)
	// Keep the lifecycle predicate on the write itself. A terminal transition
	// can commit after the timestamp read but before this UPDATE; without the
	// predicate that race would resurrect reconciliation clocks on a terminal
	// row after the terminal writer cleared them.
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET reconcile_at = ?, last_reconcile_at = ?, next_reconcile_at = ?, updated_at = ? WHERE proxy_job_id = ? AND state = ? AND terminal_at IS NULL`,
		formatTime(at), formatTime(at), formatTime(next), formatTime(at), proxyID, StateUncertain)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		var currentState string
		var terminalAt sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state, terminal_at FROM jobs WHERE proxy_job_id = ?`, proxyID).
			Scan(&currentState, &terminalAt); err != nil {
			return err
		}
		if terminalAt.Valid || IsTerminalState(currentState) || currentState != StateUncertain {
			return ErrInvalidTransition
		}
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func reconciliationBackoff(previousReconcile, previousNext time.Time) time.Duration {
	backoff := reconciliationInitialBackoff
	if previousReconcile.IsZero() || previousNext.IsZero() || !previousNext.After(previousReconcile) {
		return backoff
	}
	previousDelay := previousNext.Sub(previousReconcile)
	if previousDelay >= reconciliationMaxBackoff {
		return reconciliationMaxBackoff
	}
	if previousDelay > reconciliationMaxBackoff/2 {
		return reconciliationMaxBackoff
	}
	doubled := previousDelay * 2
	if doubled > backoff {
		backoff = doubled
	}
	if backoff > reconciliationMaxBackoff {
		return reconciliationMaxBackoff
	}
	return backoff
}

func (s *Store) MarkReconciled(ctx context.Context, args ...any) error {
	proxyID, at, err := parseIDTimeArgs(args)
	if err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET last_reconcile_at = ?, reconcile_at = NULL, next_reconcile_at = NULL, updated_at = ? WHERE proxy_job_id = ?`,
		formatTime(at), formatTime(at), proxyID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) ScheduleReconciliation(ctx context.Context, args ...any) error {
	proxyID, at, err := parseIDTimeArgs(args)
	if err != nil {
		return err
	}
	if at.IsZero() {
		return ErrInvalidArgument
	}
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET reconcile_at = ?, next_reconcile_at = ?, updated_at = ? WHERE proxy_job_id = ?`,
		formatTime(at), formatTime(at), formatTime(time.Now().UTC()), proxyID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// CleanupTerminalBefore removes only terminal rows older than cutoff. A
// duration argument is interpreted as "older than now minus duration"; a
// time.Time is used directly. The explicit cutoff form makes the 30-day
// policy configurable without baking policy into the database layer.
func (s *Store) CleanupTerminalBefore(ctx context.Context, cutoff any) (int, error) {
	cutoffTime, err := parseCleanupCutoff(cutoff)
	if err != nil {
		return 0, err
	}
	terminalStates := []string{StateTerminal, StateCompleted, StateCanceled, StateCancelled, StateAborted, StateFailed, StateStopped}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(terminalStates)), ",")
	args := make([]any, 0, len(terminalStates)+1)
	for _, state := range terminalStates {
		args = append(args, state)
	}
	args = append(args, formatTime(cutoffTime))
	query := `DELETE FROM jobs WHERE state IN (` + placeholders + `) AND COALESCE(terminal_at, updated_at) < ?`
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	return int(count), err
}

func (s *Store) Cleanup(ctx context.Context, cutoff any) (int, error) {
	return s.CleanupTerminalBefore(ctx, cutoff)
}

func (s *Store) CleanupExpired(ctx context.Context, now time.Time, retention time.Duration) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if retention <= 0 {
		retention = DefaultRetention
	}
	return s.CleanupTerminalBefore(ctx, now.Add(-retention))
}

func parseReservationArgs(args []any) (Job, error) {
	if len(args) == 1 {
		if job, ok := args[0].(Job); ok {
			return job, nil
		}
	}
	// Convenience form for small adapters: queue, requesting user, job name,
	// document format, and optional copies. All fields after queue/user are
	// optional and default to the zero values of Job.
	if len(args) >= 2 && len(args) <= 5 {
		queue, queueOK := asString(args[0])
		user, userOK := asString(args[1])
		if !queueOK || !userOK {
			return Job{}, ErrInvalidArgument
		}
		job := Job{Queue: queue, RequestingUser: user}
		if len(args) >= 3 {
			job.JobName, queueOK = asString(args[2])
			if !queueOK {
				return Job{}, ErrInvalidArgument
			}
		}
		if len(args) >= 4 {
			job.DocumentFormat, queueOK = asString(args[3])
			if !queueOK {
				return Job{}, ErrInvalidArgument
			}
		}
		if len(args) == 5 {
			job.Copies, queueOK = asInt(args[4])
			if !queueOK {
				return Job{}, ErrInvalidArgument
			}
		}
		return job, nil
	}
	return Job{}, ErrInvalidArgument
}

func parseScopedID(args []any) (string, int, error) {
	if len(args) == 1 {
		id, ok := asInt(args[0])
		if ok {
			return "", id, nil
		}
	}
	if len(args) == 2 {
		queue, ok := asString(args[0])
		id, idOK := asInt(args[1])
		if ok && idOK {
			return queue, id, nil
		}
	}
	return "", 0, ErrInvalidArgument
}

func parseScopedString(args []any) (string, string, error) {
	if len(args) == 1 {
		value, ok := asString(args[0])
		if ok {
			return "", value, nil
		}
	}
	if len(args) == 2 {
		queue, ok := asString(args[0])
		value, valueOK := asString(args[1])
		if ok && valueOK {
			return queue, value, nil
		}
	}
	return "", "", ErrInvalidArgument
}

func parseMappingArgs(args []any) (string, int, int, string, error) {
	queue := ""
	if len(args) == 4 {
		var ok bool
		queue, ok = asString(args[0])
		if !ok {
			return "", 0, 0, "", ErrInvalidArgument
		}
		args = args[1:]
	}
	if len(args) != 3 {
		return "", 0, 0, "", ErrInvalidArgument
	}
	proxyID, ok := asInt(args[0])
	if !ok {
		return "", 0, 0, "", ErrInvalidArgument
	}
	upstreamID, ok := asInt(args[1])
	if !ok {
		return "", 0, 0, "", ErrInvalidArgument
	}
	upstreamURI, ok := asString(args[2])
	if !ok {
		return "", 0, 0, "", ErrInvalidArgument
	}
	return queue, proxyID, upstreamID, upstreamURI, nil
}

func parseUncertainArgs(args []any) (string, int, string, time.Time, error) {
	queue := ""
	if len(args) >= 2 {
		if value, ok := asString(args[0]); ok {
			if _, idOK := asInt(args[1]); idOK {
				queue = value
				args = args[1:]
			}
		}
	}
	if len(args) < 1 {
		return "", 0, "", time.Time{}, ErrInvalidArgument
	}
	proxyID, ok := asInt(args[0])
	if !ok {
		return "", 0, "", time.Time{}, ErrInvalidArgument
	}
	reason := ""
	at := time.Time{}
	for _, arg := range args[1:] {
		switch value := arg.(type) {
		case time.Time:
			at = value
		case string:
			if reason == "" {
				reason = value
			}
		default:
			return "", 0, "", time.Time{}, ErrInvalidArgument
		}
	}
	return queue, proxyID, reason, at, nil
}

func parseObservedArgs(args []any) (string, int, string, string, time.Time, error) {
	queue := ""
	if len(args) >= 3 {
		if value, ok := asString(args[0]); ok {
			if _, idOK := asInt(args[1]); idOK {
				queue = value
				args = args[1:]
			}
		}
	}
	if len(args) < 2 {
		return "", 0, "", "", time.Time{}, ErrInvalidArgument
	}
	proxyID, ok := asInt(args[0])
	if !ok {
		return "", 0, "", "", time.Time{}, ErrInvalidArgument
	}
	observedState, ok := asString(args[1])
	if !ok {
		return "", 0, "", "", time.Time{}, ErrInvalidArgument
	}
	observedError := ""
	at := time.Time{}
	for _, arg := range args[2:] {
		switch value := arg.(type) {
		case time.Time:
			at = value
		case string:
			if observedError == "" {
				observedError = value
			}
		default:
			return "", 0, "", "", time.Time{}, ErrInvalidArgument
		}
	}
	return queue, proxyID, observedState, observedError, at, nil
}

func parseTerminalArgs(args []any) (string, int, string, string, string, time.Time, error) {
	queue := ""
	if len(args) >= 2 {
		if value, ok := asString(args[0]); ok {
			if _, idOK := asInt(args[1]); idOK {
				queue = value
				args = args[1:]
			}
		}
	}
	if len(args) < 1 {
		return "", 0, "", "", "", time.Time{}, ErrInvalidArgument
	}
	proxyID, ok := asInt(args[0])
	if !ok {
		return "", 0, "", "", "", time.Time{}, ErrInvalidArgument
	}
	terminalState := ""
	observedState := ""
	observedError := ""
	var at time.Time
	stringsSeen := 0
	for _, arg := range args[1:] {
		switch value := arg.(type) {
		case time.Time:
			at = value
		case string:
			switch stringsSeen {
			case 0:
				terminalState = value
			case 1:
				// With the compact (state, error) form the second string
				// is an error. A third string unambiguously supplies an
				// observed state before that error.
				observedError = value
			case 2:
				observedError = value
			}
			stringsSeen++
		default:
			return "", 0, "", "", "", time.Time{}, ErrInvalidArgument
		}
	}
	if stringsSeen >= 3 {
		// The three-string form is (terminal state, observed state, error).
		values := make([]string, 0, 3)
		for _, arg := range args[1:] {
			if value, ok := arg.(string); ok {
				values = append(values, value)
			}
		}
		terminalState, observedState, observedError = values[0], values[1], values[2]
	}
	return queue, proxyID, terminalState, observedState, observedError, at, nil
}

func parseDocumentArgs(args []any) (string, int, string, int64, *int, int, *int, bool, error) {
	queue := ""
	if len(args) >= 2 {
		if value, ok := asString(args[0]); ok {
			if _, idOK := asInt(args[1]); idOK {
				queue = value
				args = args[1:]
			}
		}
	}
	// proxyID, format, bytes, page-count, copies, impressions, last-document
	if len(args) != 7 {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	proxyID, ok := asInt(args[0])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	format, ok := asString(args[1])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	payloadBytes, ok := asInt64(args[2])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	pageCount, ok := asOptionalInt(args[3])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	copies, ok := asInt(args[4])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	impressions, ok := asOptionalInt(args[5])
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	lastDocument, ok := args[6].(bool)
	if !ok {
		return "", 0, "", 0, nil, 0, nil, false, ErrInvalidArgument
	}
	return queue, proxyID, format, payloadBytes, pageCount, copies, impressions, lastDocument, nil
}

func parseJobFilterArgs(args []any) (JobFilter, error) {
	filter := JobFilter{}
	for len(args) > 0 {
		switch value := args[0].(type) {
		case JobFilter:
			filter = mergeJobFilters(filter, value)
			args = args[1:]
		case string:
			if filter.Queue == "" {
				filter.Queue = value
			} else if filter.RequestingUser == "" {
				filter.RequestingUser = value
			} else {
				return JobFilter{}, ErrInvalidArgument
			}
			args = args[1:]
		default:
			return JobFilter{}, ErrInvalidArgument
		}
	}
	return filter, nil
}

func mergeJobFilters(base, extra JobFilter) JobFilter {
	if extra.Queue != "" {
		base.Queue = extra.Queue
	}
	if extra.QueueOwner != "" {
		base.QueueOwner = extra.QueueOwner
	}
	if extra.RequestingUser != "" {
		base.RequestingUser = extra.RequestingUser
	}
	if extra.User != "" {
		base.User = extra.User
	}
	if extra.RequestingUserName != "" {
		base.RequestingUserName = extra.RequestingUserName
	}
	if extra.State != "" {
		base.State = extra.State
	}
	if len(extra.States) > 0 {
		base.States = extra.States
	}
	if extra.ObservedState != "" {
		base.ObservedState = extra.ObservedState
	}
	if extra.ProxyJobID != 0 {
		base.ProxyJobID = extra.ProxyJobID
	}
	if extra.UpstreamJobID != 0 {
		base.UpstreamJobID = extra.UpstreamJobID
	}
	if !extra.CreatedAfter.IsZero() {
		base.CreatedAfter = extra.CreatedAfter
	}
	if !extra.CreatedBefore.IsZero() {
		base.CreatedBefore = extra.CreatedBefore
	}
	if !extra.UpdatedAfter.IsZero() {
		base.UpdatedAfter = extra.UpdatedAfter
	}
	if !extra.UpdatedBefore.IsZero() {
		base.UpdatedBefore = extra.UpdatedBefore
	}
	if !extra.TerminalBefore.IsZero() {
		base.TerminalBefore = extra.TerminalBefore
	}
	if !extra.TerminalAfter.IsZero() {
		base.TerminalAfter = extra.TerminalAfter
	}
	if extra.IncludeTerminal {
		base.IncludeTerminal = true
	}
	if extra.ExcludeTerminal {
		base.ExcludeTerminal = true
	}
	if extra.Limit != 0 {
		base.Limit = extra.Limit
	}
	if extra.Offset != 0 {
		base.Offset = extra.Offset
	}
	return base
}

func parseReconcileArgs(args []any) (string, time.Time, int, error) {
	queue := ""
	at := time.Now().UTC()
	limit := 100
	if len(args) > 0 {
		if value, ok := asString(args[0]); ok {
			queue = value
			args = args[1:]
		}
	}
	for _, arg := range args {
		switch value := arg.(type) {
		case time.Time:
			at = value
		case int:
			limit = value
		case int64:
			limit = int(value)
		default:
			return "", time.Time{}, 0, ErrInvalidArgument
		}
	}
	return queue, at, limit, nil
}

func parseIDTimeArgs(args []any) (int, time.Time, error) {
	if len(args) == 0 || len(args) > 2 {
		return 0, time.Time{}, ErrInvalidArgument
	}
	id, ok := asInt(args[0])
	if !ok {
		return 0, time.Time{}, ErrInvalidArgument
	}
	at := time.Time{}
	if len(args) == 2 {
		value, ok := args[1].(time.Time)
		if !ok {
			return 0, time.Time{}, ErrInvalidArgument
		}
		at = value
	}
	return id, at, nil
}

func parseCleanupCutoff(value any) (time.Time, error) {
	switch cutoff := value.(type) {
	case time.Time:
		if cutoff.IsZero() {
			return time.Now().UTC().Add(-DefaultRetention), nil
		}
		return cutoff, nil
	case time.Duration:
		if cutoff <= 0 {
			cutoff = DefaultRetention
		}
		return time.Now().UTC().Add(-cutoff), nil
	case nil:
		return time.Now().UTC().Add(-DefaultRetention), nil
	default:
		return time.Time{}, ErrInvalidArgument
	}
}

func asInt(value any) (int, bool) {
	switch value := value.(type) {
	case int:
		return value, true
	case int8:
		return int(value), true
	case int16:
		return int(value), true
	case int32:
		return int(value), true
	case int64:
		return int(value), true
	case uint:
		return int(value), true
	case uint8:
		return int(value), true
	case uint16:
		return int(value), true
	case uint32:
		return int(value), true
	case uint64:
		return int(value), true
	default:
		return 0, false
	}
}

func asInt64(value any) (int64, bool) {
	if intValue, ok := asInt(value); ok {
		return int64(intValue), true
	}
	if value, ok := value.(int64); ok {
		return value, true
	}
	return 0, false
}

func asString(value any) (string, bool) {
	stringValue, ok := value.(string)
	return stringValue, ok
}

func asOptionalInt(value any) (*int, bool) {
	if value == nil {
		return nil, true
	}
	if pointer, ok := value.(*int); ok {
		return pointer, true
	}
	intValue, ok := asInt(value)
	if !ok {
		return nil, false
	}
	return &intValue, true
}

func nullableUpstreamID(job Job) any {
	if job.HasUpstreamJobID || job.UpstreamJobID != 0 {
		return job.UpstreamJobID
	}
	return nil
}

func nullableUpstreamURI(job Job) any {
	if job.HasUpstreamJobURI || job.UpstreamJobURI != "" {
		return job.UpstreamJobURI
	}
	return nil
}

func nullableIntValue(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableStringValue(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value sql.NullString) time.Time {
	if !value.Valid || value.String == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		if parsed, err = time.Parse(time.RFC3339, value.String); err != nil {
			return time.Time{}
		}
	}
	return parsed
}

func parseNullableTime(value sql.NullString) *time.Time {
	parsed := parseTime(value)
	if parsed.IsZero() {
		return nil
	}
	return &parsed
}

func nullableTime(value *time.Time) any {
	if value == nil || value.IsZero() {
		return nil
	}
	return formatTime(*value)
}

func timeOrNow(value time.Time) time.Time {
	if value.IsZero() {
		return time.Now().UTC()
	}
	return value
}

func isUniqueViolation(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique") || strings.Contains(message, "constraint")
}
