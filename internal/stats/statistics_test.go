package stats

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testCollector(t *testing.T) *Collector {
	t.Helper()
	c := New(Options{Enabled: true, Path: filepath.Join(t.TempDir(), "stats.sqlite"), QueueSize: 64, BatchSize: 32, FlushInterval: time.Hour, DetailRetention: 90 * 24 * time.Hour, CPURetention: 30 * 24 * time.Hour, Resources: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if c.db == nil {
		t.Fatalf("open: %+v", c.Health())
	}
	t.Cleanup(func() {
		if e := c.Close(); e != nil {
			t.Error(e)
		}
	})
	return c
}
func persistTest(t *testing.T, c *Collector, jobs ...Job) {
	t.Helper()
	var events []event
	for _, j := range jobs {
		events = append(events, event{kind: eventJob, job: j})
	}
	if e := c.writeBatch(events); e != nil {
		t.Fatal(e)
	}
}
func scalar(t *testing.T, c *Collector, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if e := c.db.QueryRow(q, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func testReport(t *testing.T, c *Collector, o ReportOptions) Report {
	t.Helper()
	r, e := Query(context.Background(), c.opts.Path, o)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func intp(n int) *int { return &n }

func TestJobAccountingIsIdempotentAndKeepsUnknowns(t *testing.T) {
	c := testCollector(t)
	now := time.Now().UTC()
	j := Job{Register: true, RegistryID: "registry", JobID: 1, Queue: "office", User: "alice", CreatedAt: now, UpdatedAt: now, State: "reserved", Copies: 2, EffectiveColor: "monochrome"}
	persistTest(t, c, j, j)
	j.Register = false
	j.Accepted = true
	j.State = "mapped"
	j.DocumentCount = 1
	j.PayloadBytes = 10
	j.PageCount = intp(2)
	j.EstimatedImpressions = intp(4)
	j.UpdatedAt = now.Add(time.Second)
	persistTest(t, c, j, j)
	r := testReport(t, c, ReportOptions{Command: "summary"})
	if len(r.Rows) != 1 || number(r.Rows[0]["job_count"]) != 1 || r.Rows[0]["completed_impressions"] != nil || number(r.Rows[0]["completed_impressions_known"]) != 0 {
		t.Fatalf("unknowns/counts: %+v", r.Rows)
	}
	j.ObservedState = "completed"
	j.CompletedImpressions = intp(4)
	j.CompletedSheets = intp(2)
	j.TerminalAt = &now
	j.UpdatedAt = now.Add(2 * time.Second)
	persistTest(t, c, j, j)
	stale := j
	stale.User = "mallory"
	stale.Queue = "elsewhere"
	stale.CompletedImpressions = intp(1)
	stale.CompletedSheets = nil
	stale.ObservedState = "processing"
	stale.UpdatedAt = now
	persistTest(t, c, stale)
	r = testReport(t, c, ReportOptions{Command: "users"})
	if len(r.Rows) != 1 || r.Rows[0]["user_name"] != "alice" || number(r.Rows[0]["completed_count"]) != 1 || number(r.Rows[0]["completed_impressions"]) != 4 || number(r.Rows[0]["payload_bytes"]) != 10 {
		t.Fatalf("merged stats: %+v", r.Rows)
	}
	unknown := j
	unknown.JobID = 2
	unknown.Register = false
	persistTest(t, c, unknown)
	if scalar(t, c, `SELECT SUM(job_count) FROM job_rollups`) != 1 {
		t.Fatal("legacy job backfilled")
	}
	unknown.Register = true
	unknown.RegistryID = "replacement"
	unknown.User = ""
	unknown.CompletedImpressions = nil
	unknown.CompletedSheets = nil
	unknown.ObservedState = ""
	unknown.State = "reserved"
	persistTest(t, c, unknown)
	r = testReport(t, c, ReportOptions{Command: "users", UserSet: true, User: ""})
	if len(r.Rows) != 1 || number(r.Rows[0]["job_count"]) != 1 {
		t.Fatalf("exact unknown user: %+v", r.Rows)
	}
}

func TestRetentionAndLateObservation(t *testing.T) {
	c := testCollector(t)
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	j := Job{Register: true, RegistryID: "r", JobID: 1, Queue: "q", CreatedAt: old, UpdatedAt: old, Accepted: true, State: "mapped", EffectiveSides: "two-sided-long-edge"}
	persistTest(t, c, j)
	if e := c.writeBatch([]event{{kind: eventAction, action: Action{ID: "old-action", RunID: c.RunID(), Kind: "client-request", StartedAt: old, FinishedAt: old, ClientBytes: 7}}}); e != nil {
		t.Fatal(e)
	}
	c.Cleanup(now)
	if scalar(t, c, `SELECT COUNT(*) FROM jobs`) != 0 || scalar(t, c, `SELECT COUNT(*) FROM actions`) != 0 || scalar(t, c, `SELECT COUNT(*) FROM job_checkpoints`) != 1 {
		t.Fatal("detail not compacted")
	}
	j.Register = false
	j.UpdatedAt = now
	j.ObservedState = "completed"
	j.TerminalAt = &now
	j.CompletedImpressions = intp(3)
	persistTest(t, c, j, j)
	if scalar(t, c, `SELECT SUM(job_count) FROM job_rollups`) != 1 || scalar(t, c, `SELECT SUM(completed_count) FROM job_rollups`) != 1 || scalar(t, c, `SELECT SUM(unresolved_count) FROM job_rollups`) != 0 || scalar(t, c, `SELECT SUM(job_count) FROM job_setting_rollups WHERE effective_sides='two-sided-long-edge'`) != 1 {
		t.Fatal("late completion corrupted rollups")
	}
	if scalar(t, c, `SELECT SUM(action_count) FROM action_rollups`) != 1 {
		t.Fatal("action history lost")
	}
	c.Cleanup(now.Add(101 * 24 * time.Hour))
	if scalar(t, c, `SELECT COUNT(*) FROM job_checkpoints`) != 0 {
		t.Fatal("terminal checkpoint retained")
	}
	persistTest(t, c, j)
	if scalar(t, c, `SELECT SUM(job_count) FROM job_rollups`) != 1 {
		t.Fatal("late observation recreated retired job")
	}
}

func TestActionsDeduplicateAndCPUDoesNotOverlap(t *testing.T) {
	c := testCollector(t)
	now := time.Now().UTC()
	a := Action{ID: "a", RunID: c.RunID(), Kind: "translation", StartedAt: now, FinishedAt: now, DurationNS: 100, BufferPeakBytes: 2048}
	if e := c.writeBatch([]event{{kind: eventAction, action: a}, {kind: eventAction, action: a}}); e != nil {
		t.Fatal(e)
	}
	if scalar(t, c, `SELECT SUM(action_count) FROM action_rollups`) != 1 {
		t.Fatal("action double counted")
	}
	tx, e := c.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	for _, reading := range []CPUReading{{true, 1, 1, now}, {true, 3, 2, now.Add(time.Second)}, {true, 2, 1, now.Add(time.Millisecond)}, {true, 3, 2, now.Add(time.Second)}, {true, 4, 3, now.Add(2 * time.Second)}} {
		if e := persistCPU(tx, "cpu", reading); e != nil {
			t.Fatal(e)
		}
	}
	if e := tx.Commit(); e != nil {
		t.Fatal(e)
	}
	var cpu float64
	if e := c.db.QueryRow(`SELECT SUM(user_seconds+system_seconds) FROM cpu_daily`).Scan(&cpu); e != nil {
		t.Fatal(e)
	}
	if cpu != 5 {
		t.Fatalf("overlapping CPU: %v", cpu)
	}
	r := testReport(t, c, ReportOptions{Command: "resources", UserSet: true, User: "alice"})
	if _, ok := r.Metadata["cpu_omitted"]; !ok {
		t.Fatal("per-user CPU was not omitted")
	}
}

func TestDisabledFailureAndConcurrentClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled.sqlite")
	disabled := New(Options{Path: path}, nil)
	_, span := Begin(context.Background(), disabled, Action{})
	if span != nil {
		t.Fatal("disabled span allocated")
	}
	disabled.RecordAction(Action{})
	disabled.Cleanup(time.Now())
	_ = disabled.Close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled created DB: %v", err)
	}
	failed := New(Options{Enabled: true, Path: filepath.Join(t.TempDir(), "missing", "stats.sqlite")}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	failed.RecordAction(Action{})
	if failed.Health().Errors == 0 || failed.Health().Dropped != 1 {
		t.Fatalf("failure not exposed: %+v", failed.Health())
	}
	_ = failed.Close()
	c := testCollector(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				c.RecordAction(Action{Kind: "concurrent"})
			}
		}()
	}
	wg.Wait()
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	r, e := Query(context.Background(), c.opts.Path, ReportOptions{Command: "actions", Limit: 1000})
	if e != nil {
		t.Fatal(e)
	}
	if uint64(len(r.Rows))+c.Health().Dropped != 800 {
		t.Fatalf("silent loss: rows=%d health=%+v", len(r.Rows), c.Health())
	}
}

func TestSQLiteLockDoesNotBlockRecorders(t *testing.T) {
	c := testCollector(t)
	other, e := sql.Open("sqlite", c.opts.Path)
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	if _, e = other.Exec(`BEGIN IMMEDIATE`); e != nil {
		t.Fatal(e)
	}
	started := time.Now()
	for range 500 {
		c.RecordAction(Action{Kind: "locked"})
	}
	if time.Since(started) > time.Second {
		t.Fatal("recording waited for SQLite")
	}
	if _, e = other.Exec(`ROLLBACK`); e != nil {
		t.Fatal(e)
	}
}

type memoryRecorder struct {
	mu      sync.Mutex
	actions []Action
}

func (r *memoryRecorder) RecordAction(a Action) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.actions = append(r.actions, a)
}
func TestResourceLifetimesAndParentAccounting(t *testing.T) {
	r := &memoryRecorder{}
	ctx, parent := Begin(context.Background(), r, Action{Kind: "client-request", User: "alice"})
	ctx, child := BeginChild(ctx, Action{Kind: "translation"})
	a := child.AcquireBuffer("scratch", 384)
	b := child.AcquireBuffer("header", 1796)
	a.Release()
	b.Release()
	temp := child.AcquireTemp("output", 0)
	temp.Written(200)
	temp.SetSize(100)
	child.Finish("success")
	_, other := BeginChild(ctx, Action{Kind: "upstream-request"})
	second := other.AcquireTemp("second", 50)
	second.Release()
	other.Finish("success")
	temp.Release()
	parent.Finish("success")
	parent.Finish("duplicate")
	if len(r.actions) != 3 {
		t.Fatalf("finished twice: %d", len(r.actions))
	}
	p := r.actions[2]
	if p.BufferPeakBytes != 2180 || p.TempPeakBytes != 150 || p.TempWrittenBytes != 200 {
		t.Fatalf("parent accounting: %+v", p)
	}
	if r.actions[0].User != "alice" || r.actions[0].ParentID != parent.ID() {
		t.Fatal("identity lost")
	}
}

func BenchmarkDisabledAction(b *testing.B) {
	c := New(Options{}, nil)
	b.ReportAllocs()
	for b.Loop() {
		_, s := Begin(context.Background(), c, Action{})
		s.Finish("success")
	}
}
