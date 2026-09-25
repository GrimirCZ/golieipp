package stats

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	defaultQueueSize     = 4096
	defaultBatchSize     = 256
	defaultFlushInterval = time.Second
	busyTimeout          = 250 * time.Millisecond
	closeTimeout         = 5 * time.Second
)

type eventKind uint8

const (
	eventAction eventKind = iota + 1
	eventJob
)

type event struct {
	kind   eventKind
	action Action
	job    Job
	cpu    CPUReading
	hasCPU bool
}

// Collector is an intentionally best-effort writer. Printing paths call its
// methods without waiting for SQLite; a full queue increments Dropped and
// never blocks a request handler.
type Collector struct {
	opts   Options
	logger *slog.Logger
	db     *sql.DB
	runID  string

	queue chan event
	stop  chan struct{}
	done  chan struct{}

	closed   atomic.Bool
	acceptMu sync.RWMutex
	closeErr atomic.Value // stores error, if any

	healthMu sync.RWMutex
	health   Health

	writeMu  sync.Mutex
	warnMu   sync.Mutex
	lastWarn time.Time

	startCPU CPUReading
}

// New creates a collector. It deliberately has no error return: statistics
// are optional and a bad path must never prevent the proxy from printing.
// Initialization failures are reflected in Health and logged; subsequent
// calls become no-ops with dropped records.
func New(options Options, logger *slog.Logger) *Collector {
	if logger == nil {
		logger = slog.Default()
	}
	c := &Collector{
		opts:   options,
		logger: logger,
		runID:  uuid.NewString(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
		health: Health{Enabled: options.Enabled},
	}
	if !options.Enabled {
		close(c.done)
		return c
	}
	if c.opts.QueueSize <= 0 {
		c.opts.QueueSize = defaultQueueSize
	}
	if c.opts.BatchSize <= 0 || c.opts.BatchSize > c.opts.QueueSize {
		c.opts.BatchSize = defaultBatchSize
		if c.opts.BatchSize > c.opts.QueueSize {
			c.opts.BatchSize = c.opts.QueueSize
		}
	}
	if c.opts.FlushInterval <= 0 {
		c.opts.FlushInterval = defaultFlushInterval
	}

	path := c.opts.Path
	if path == "" {
		path = "stats.sqlite"
	}
	// filepath.Clean is useful for logs and leaves SQLite URI paths intact.
	if !strings.HasPrefix(path, "file:") {
		path = filepath.Clean(path)
	}
	db, err := sql.Open("sqlite", path)
	if err == nil {
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		err = configureDB(db)
	}
	if err == nil {
		err = migrate(context.Background(), db)
	}
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		c.recordError(err)
		c.warn("statistics initialization failed", err)
		close(c.done)
		return c
	}
	c.db = db
	c.initializeMetadata()
	if c.opts.CPU {
		c.startCPU = ReadCPU()
	}
	if err := c.insertRun(); err != nil {
		c.recordError(err)
		c.warn("statistics run initialization failed", err)
	}
	c.queue = make(chan event, c.opts.QueueSize)
	go c.writerLoop()
	return c
}

func configureDB(db *sql.DB) error {
	for _, pragma := range []string{
		fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeout.Milliseconds()),
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return err
		}
	}
	return nil
}

func (c *Collector) insertRun() error {
	if c == nil || c.db == nil {
		return errors.New("statistics database is unavailable")
	}
	started := time.Now().UTC()
	_, err := c.db.Exec(`INSERT INTO runs(run_id, started_at, cpu_start_available,
cpu_start_user_seconds, cpu_start_system_seconds) VALUES(?,?,?,?,?)`,
		c.runID, formatTime(started), boolInt(c.startCPU.Available), nullableFloat(c.startCPU, true), nullableFloat(c.startCPU, false))
	if err == nil && c.startCPU.Available {
		_, err = c.db.Exec(`INSERT INTO cpu_cursors(run_id,last_at,available,user_seconds,system_seconds) VALUES(?,?,?,?,?)`, c.runID, formatTime(c.startCPU.At), 1, c.startCPU.UserSeconds, c.startCPU.SystemSeconds)
	}
	return err
}

func nullableFloat(cpu CPUReading, user bool) any {
	if !cpu.Available {
		return nil
	}
	if user {
		return cpu.UserSeconds
	}
	return cpu.SystemSeconds
}

// RunID identifies this process's recording run. It is empty only for a
// disabled collector's zero-like callers; New always creates one internally.
func (c *Collector) RunID() string {
	if c == nil {
		return ""
	}
	return c.runID
}

func (c *Collector) ResourcesEnabled() bool {
	return c != nil && c.opts.Enabled && c.opts.Resources
}

func (c *Collector) Enabled() bool { return c != nil && c.opts.Enabled }

func (c *Collector) CPUEnabled() bool {
	return c != nil && c.opts.Enabled && c.opts.CPU
}

// Health returns a point-in-time writer health snapshot.
func (c *Collector) Health() Health {
	if c == nil {
		return Health{}
	}
	c.healthMu.RLock()
	defer c.healthMu.RUnlock()
	return c.health
}

func (c *Collector) recordDropped() {
	c.healthMu.Lock()
	c.health.Dropped++
	c.healthMu.Unlock()
}

func (c *Collector) recordError(err error) {
	if err == nil {
		return
	}
	c.healthMu.Lock()
	c.health.Errors++
	c.health.LastError = err.Error()
	c.healthMu.Unlock()
}

func (c *Collector) recordPersisted() {
	c.healthMu.Lock()
	c.health.LastPersisted = time.Now().UTC()
	c.healthMu.Unlock()
}

func (c *Collector) warn(message string, err error) {
	if c == nil || c.logger == nil || err == nil {
		return
	}
	c.warnMu.Lock()
	defer c.warnMu.Unlock()
	now := time.Now()
	if !c.lastWarn.IsZero() && now.Sub(c.lastWarn) < time.Minute {
		return
	}
	c.lastWarn = now
	c.logger.Warn(message, "error", err)
}

// RecordAction queues one completed action. Empty IDs and timestamps are
// filled here so retries and duplicate suppression remain deterministic.
func (c *Collector) RecordAction(action Action) {
	if c == nil || !c.opts.Enabled {
		return
	}
	now := time.Now().UTC()
	if action.ID == "" {
		action.ID = uuid.NewString()
	}
	if action.RunID == "" {
		action.RunID = c.runID
	}
	if action.StartedAt.IsZero() {
		action.StartedAt = now
	}
	if action.FinishedAt.IsZero() {
		action.FinishedAt = now
	}
	action.StartedAt = action.StartedAt.UTC()
	action.FinishedAt = action.FinishedAt.UTC()
	if action.DurationNS <= 0 && !action.FinishedAt.Before(action.StartedAt) {
		action.DurationNS = action.FinishedAt.Sub(action.StartedAt).Nanoseconds()
	}
	e := event{kind: eventAction, action: action}
	if action.BufferComponents != nil {
		e.action.BufferComponents = make(map[string]int64, len(action.BufferComponents))
		for key, value := range action.BufferComponents {
			e.action.BufferComponents[key] = value
		}
	}
	if c.opts.CPU {
		e.cpu = ReadCPU()
		e.hasCPU = true
	}
	c.enqueue(e)
}

// ObserveJob queues a complete proxy-job accounting snapshot. An unknown job
// is accepted only when Register is set; this is the no-backfill boundary.
func (c *Collector) ObserveJob(job Job) {
	if c == nil || !c.opts.Enabled {
		return
	}
	now := time.Now().UTC()
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	if job.UpdatedAt.IsZero() {
		job.UpdatedAt = now
	}
	job.CreatedAt = job.CreatedAt.UTC()
	job.UpdatedAt = job.UpdatedAt.UTC()
	job.PageCount = cloneInt(job.PageCount)
	job.EstimatedImpressions = cloneInt(job.EstimatedImpressions)
	job.CompletedImpressions = cloneInt(job.CompletedImpressions)
	job.CompletedSheets = cloneInt(job.CompletedSheets)
	if job.ProcessingAt != nil {
		t := job.ProcessingAt.UTC()
		job.ProcessingAt = &t
	}
	if job.TerminalAt != nil {
		t := job.TerminalAt.UTC()
		job.TerminalAt = &t
	}
	c.enqueue(event{kind: eventJob, job: job})
}

func (c *Collector) enqueue(e event) {
	c.acceptMu.RLock()
	defer c.acceptMu.RUnlock()
	if c.queue == nil || c.closed.Load() {
		c.recordDropped()
		return
	}
	select {
	case c.queue <- e:
	default:
		c.recordDropped()
		c.warn("statistics queue full; record dropped", errors.New("recording queue capacity exceeded"))
	}
}

func (c *Collector) writerLoop() {
	defer close(c.done)
	defer c.db.Close()
	ticker := time.NewTicker(c.opts.FlushInterval)
	defer ticker.Stop()
	pending := make([]event, 0, c.opts.BatchSize)
	savedHealth := c.Health()
	flush := func() {
		if len(pending) == 0 {
			h := c.Health()
			if h.Dropped == savedHealth.Dropped && h.Errors == savedHealth.Errors {
				return
			}
		}
		batch := pending
		pending = make([]event, 0, c.opts.BatchSize)
		if err := c.writeBatchWithRetry(batch); err != nil {
			c.warn("statistics persistence failed", err)
			c.recordDroppedN(len(batch))
		} else {
			savedHealth = c.Health()
		}
	}
	for {
		select {
		case e := <-c.queue:
			pending = append(pending, e)
			if len(pending) >= c.opts.BatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-c.stop:
			// Do not close queue: in-flight RecordAction calls can still have
			// selected the send case. Drain all currently visible records, then
			// perform a bounded final flush.
			for {
				select {
				case e := <-c.queue:
					pending = append(pending, e)
					if len(pending) >= c.opts.BatchSize {
						flush()
					}
				default:
					flush()
					c.finishRun()
					return
				}
			}
		}
	}
}

func (c *Collector) recordDroppedN(n int) {
	if n <= 0 {
		return
	}
	c.healthMu.Lock()
	c.health.Dropped += uint64(n)
	c.healthMu.Unlock()
}

func (c *Collector) writeBatchWithRetry(batch []event) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*25) * time.Millisecond)
		}
		err = c.writeBatch(batch)
		if err == nil {
			c.recordPersisted()
			return nil
		}
		c.recordError(err)
	}
	return err
}

func (c *Collector) writeBatch(batch []event) error {
	if c == nil || c.db == nil {
		return errors.New("statistics database is unavailable")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range batch {
		switch e.kind {
		case eventAction:
			if err := persistAction(tx, e.action); err != nil {
				return err
			}
			if e.hasCPU && e.cpu.Available {
				if err := persistCPU(tx, c.runID, e.cpu); err != nil {
					return err
				}
			}
		case eventJob:
			if err := persistJob(tx, e.job, c.opts.RollupRetention); err != nil {
				return err
			}
		}
	}
	if err := c.persistHealth(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return nil
}

func persistAction(tx *sql.Tx, a Action) error {
	components := ""
	if len(a.BufferComponents) != 0 {
		b, err := json.Marshal(a.BufferComponents)
		if err != nil {
			return fmt.Errorf("marshal action buffer components: %w", err)
		}
		components = string(b)
	}
	result, err := tx.Exec(`INSERT OR IGNORE INTO actions(
	id,parent_id,run_id,kind,operation,queue,user_name,owner,registry_id,job_id,
	outcome,reason,http_status,ipp_status,started_at,finished_at,duration_ns,
	client_bytes,upstream_bytes,output_bytes,buffer_peak_bytes,buffer_components_json,
	temp_peak_bytes,temp_written_bytes,concurrency,user_concurrency) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, nullableString(a.ParentID), a.RunID, a.Kind, a.Operation, a.Queue, a.User, a.Owner,
		a.RegistryID, a.JobID, a.Outcome, a.Reason, a.HTTPStatus, a.IPPStatus,
		formatTime(a.StartedAt), formatTime(a.FinishedAt), a.DurationNS, a.ClientBytes,
		a.UpstreamBytes, a.OutputBytes, a.BufferPeakBytes, components, a.TempPeakBytes,
		a.TempWrittenBytes, a.Concurrency, a.UserConcurrency)
	if err != nil {
		return fmt.Errorf("persist action %s: %w", a.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		return nil
	}
	day := dayString(a.FinishedAt)
	for component, peak := range a.BufferComponents {
		if _, err := tx.Exec(`INSERT INTO buffer_rollups(completion_day,queue,user_name,kind,component,action_count,peak_capacity_bytes) VALUES(?,?,?,?,?,1,?) ON CONFLICT(completion_day,queue,user_name,kind,component) DO UPDATE SET action_count=buffer_rollups.action_count+1,peak_capacity_bytes=MAX(buffer_rollups.peak_capacity_bytes,excluded.peak_capacity_bytes)`, day, a.Queue, a.User, a.Kind, component, peak); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO action_rollups(
	completion_day,queue,user_name,kind,operation,outcome,action_count,duration_ns,
	duration_max_ns,client_bytes,upstream_bytes,output_bytes,buffer_peak_bytes,
	temp_peak_bytes,temp_written_bytes,concurrency_max,user_concurrency_max) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(completion_day,queue,user_name,kind,operation,outcome) DO UPDATE SET
	action_count=action_rollups.action_count+excluded.action_count,
	duration_ns=action_rollups.duration_ns+excluded.duration_ns,
	duration_max_ns=MAX(action_rollups.duration_max_ns,excluded.duration_max_ns),
	client_bytes=action_rollups.client_bytes+excluded.client_bytes,
	upstream_bytes=action_rollups.upstream_bytes+excluded.upstream_bytes,
	output_bytes=action_rollups.output_bytes+excluded.output_bytes,
	buffer_peak_bytes=MAX(action_rollups.buffer_peak_bytes,excluded.buffer_peak_bytes),
	temp_peak_bytes=MAX(action_rollups.temp_peak_bytes,excluded.temp_peak_bytes),
	temp_written_bytes=action_rollups.temp_written_bytes+excluded.temp_written_bytes,
	concurrency_max=MAX(action_rollups.concurrency_max,excluded.concurrency_max),
	user_concurrency_max=MAX(action_rollups.user_concurrency_max,excluded.user_concurrency_max)`,
		day, a.Queue, a.User, a.Kind, a.Operation, a.Outcome, 1, a.DurationNS, a.DurationNS,
		a.ClientBytes, a.UpstreamBytes, a.OutputBytes, a.BufferPeakBytes, a.TempPeakBytes,
		a.TempWrittenBytes, a.Concurrency, a.UserConcurrency)
	return err
}

func persistCPU(tx *sql.Tx, runID string, reading CPUReading) error {
	if !reading.Available {
		return nil
	}
	if reading.At.IsZero() {
		reading.At = time.Now().UTC()
	}
	reading.At = reading.At.UTC()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO cpu_samples(run_id,sampled_at,available,user_seconds,system_seconds) VALUES(?,?,?,?,?)`,
		runID, formatTime(reading.At), 1, reading.UserSeconds, reading.SystemSeconds); err != nil {
		return err
	}
	var lastAt sql.NullString
	var lastAvailable int
	var lastUser, lastSystem float64
	err := tx.QueryRow(`SELECT last_at,available,user_seconds,system_seconds FROM cpu_cursors WHERE run_id=?`, runID).Scan(&lastAt, &lastAvailable, &lastUser, &lastSystem)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil && lastAt.Valid {
		previous, parseErr := time.Parse(time.RFC3339Nano, lastAt.String)
		if parseErr == nil && !reading.At.After(previous) {
			return nil
		}
	}
	if err == sql.ErrNoRows || !lastAt.Valid {
		_, err = tx.Exec(`INSERT INTO cpu_cursors(run_id,last_at,available,user_seconds,system_seconds) VALUES(?,?,?,?,?)
			ON CONFLICT(run_id) DO UPDATE SET last_at=excluded.last_at,available=excluded.available,
			user_seconds=excluded.user_seconds,system_seconds=excluded.system_seconds`,
			runID, formatTime(reading.At), 1, reading.UserSeconds, reading.SystemSeconds)
		return err
	}
	du := reading.UserSeconds - lastUser
	ds := reading.SystemSeconds - lastSystem
	if du < 0 || ds < 0 || math.IsNaN(du) || math.IsNaN(ds) || math.IsInf(du, 0) || math.IsInf(ds, 0) {
		return nil
	}
	if du < 0 || math.IsNaN(du) {
		du = 0
	}
	if ds < 0 || math.IsNaN(ds) {
		ds = 0
	}
	if _, err := tx.Exec(`INSERT INTO cpu_daily(day,user_seconds,system_seconds,sample_count) VALUES(?,?,?,?)
		ON CONFLICT(day) DO UPDATE SET user_seconds=cpu_daily.user_seconds+excluded.user_seconds,
		system_seconds=cpu_daily.system_seconds+excluded.system_seconds,
		sample_count=cpu_daily.sample_count+excluded.sample_count`,
		dayString(reading.At), du, ds, 1); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE cpu_cursors SET last_at=?,available=1,user_seconds=?,system_seconds=? WHERE run_id=?`,
		formatTime(reading.At), reading.UserSeconds, reading.SystemSeconds, runID)
	return err
}

func (c *Collector) finishRun() {
	if c == nil || c.db == nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	finished := time.Now().UTC()
	var err error
	if c.opts.CPU {
		tx, txErr := c.db.BeginTx(context.Background(), nil)
		if txErr == nil {
			reading := ReadCPU()
			if reading.Available {
				err = persistCPU(tx, c.runID, reading)
				if err == nil {
					_, err = tx.Exec(`UPDATE runs SET finished_at=?,cpu_end_available=1,cpu_end_user_seconds=?,cpu_end_system_seconds=? WHERE run_id=?`,
						formatTime(finished), reading.UserSeconds, reading.SystemSeconds, c.runID)
				}
			} else if txErr == nil {
				_, err = tx.Exec(`UPDATE runs SET finished_at=? WHERE run_id=?`, formatTime(finished), c.runID)
			}
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		} else {
			err = txErr
		}
	} else {
		_, err = c.db.Exec(`UPDATE runs SET finished_at=? WHERE run_id=?`, formatTime(finished), c.runID)
	}
	if err != nil {
		c.recordError(err)
		c.warn("statistics run finalization failed", err)
	}
	if tx, e := c.db.Begin(); e == nil {
		if e = c.persistHealth(tx); e == nil {
			_ = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
	}
}

func (c *Collector) Close() error {
	if c == nil {
		return nil
	}
	if !c.opts.Enabled {
		return nil
	}
	c.acceptMu.Lock()
	if c.closed.Swap(true) {
		c.acceptMu.Unlock()
		if value := c.closeErr.Load(); value != nil {
			return value.(error)
		}
		return nil
	}
	close(c.stop)
	c.acceptMu.Unlock()
	timer := time.NewTimer(closeTimeout)
	defer timer.Stop()
	select {
	case <-c.done:
		if c.db != nil {
			if err := c.db.Close(); err != nil {
				c.closeErr.Store(err)
				return err
			}
		}
		return nil
	case <-timer.C:
		err := errors.New("statistics shutdown timed out")
		c.closeErr.Store(err)
		c.warn("statistics shutdown timed out", err)
		return err
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func dayString(t time.Time) string {
	if t.IsZero() {
		t = time.Now().UTC()
	}
	return t.UTC().Format("2006-01-02")
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
