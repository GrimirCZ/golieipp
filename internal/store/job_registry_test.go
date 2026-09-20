package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestJobRegistryReservationAndStateTransitions(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	proxyID, err := s.Reserve(ctx, Job{
		Queue:          "office",
		RequestingUser: "alice",
		JobName:        "report.pdf",
		DocumentFormat: "application/pdf",
		Copies:         2,
	})
	if err != nil {
		t.Fatal(err)
	}

	reserved, err := s.GetByProxyID(ctx, "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.State != StateReserved || reserved.HasUpstreamJobID || reserved.HasUpstreamJobURI {
		t.Fatalf("reservation was not durable and unmapped: %+v", reserved)
	}
	if reserved.Queue != "office" || reserved.RequestingUser != "alice" {
		t.Fatalf("reservation ownership was not retained: %+v", reserved)
	}

	if err := s.MarkMapped(ctx, proxyID, 42, "ipp://printer/jobs/42"); err != nil {
		t.Fatal(err)
	}
	mapped, err := s.GetByProxyID(ctx, "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.State != StateMapped || !mapped.HasUpstreamJobID || mapped.UpstreamJobID != 42 || !mapped.HasUpstreamJobURI {
		t.Fatalf("mapping transition was not recorded: %+v", mapped)
	}

	if err := s.MarkUncertain(ctx, proxyID, "upstream response was lost"); err != nil {
		t.Fatal(err)
	}
	uncertain, err := s.GetByProxyID(ctx, "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if uncertain.State != StateUncertain || uncertain.ObservedError != "upstream response was lost" || uncertain.ReconcileAt == nil {
		t.Fatalf("uncertain transition was not recorded: %+v", uncertain)
	}

	if err := s.UpdateObserved(ctx, proxyID, "processing", ""); err != nil {
		t.Fatal(err)
	}
	observed, err := s.GetByProxyID(ctx, "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if observed.ObservedState != "processing" || observed.State != StateUncertain {
		t.Fatalf("observed state update changed lifecycle unexpectedly: %+v", observed)
	}

	if err := s.MarkTerminal(ctx, proxyID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	terminal, err := s.GetByProxyID(ctx, "office", proxyID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.State != StateTerminal || terminal.ObservedState != "completed" || terminal.TerminalAt == nil {
		t.Fatalf("terminal transition was not recorded: %+v", terminal)
	}
	if terminal.UpdatedAt.Before(terminal.CreatedAt) || terminal.TerminalAt.Before(terminal.CreatedAt) {
		t.Fatalf("timestamps regressed: %+v", terminal)
	}
}

func TestJobRegistryPersistsSelectedDocumentRoute(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	ctx := context.Background()
	route := DocumentRoute{
		ClientDocumentFormat:   "image/urf",
		UpstreamDocumentFormat: "image/pwg-raster",
		Kind:                   RouteURFToPWG,
		Mapping:                "W8->sgray_8",
		MediaName:              "iso_a4_210x297mm",
		MediaWidth:             21000,
		MediaHeight:            29700,
		MediaType:              "stationery",
		MediaSource:            7,
		ResolutionX:            600,
		ResolutionY:            600,
		PrintQuality:           5,
		Sides:                  "two-sided-long-edge",
		SheetBack:              "flipped",
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "alice", Route: route})
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	got := job.Route
	if got != route {
		t.Fatalf("selected route changed across restart: got %+v, want %+v", got, route)
	}
	if job.DocumentFormat != route.ClientDocumentFormat || job.UpstreamDocumentFormat != route.UpstreamDocumentFormat {
		t.Fatalf("document formats were not retained: %+v", job)
	}

	updated := route
	updated.ResolutionX = 300
	updated.ResolutionY = 300
	updated.SheetBack = "normal"
	if err := s.UpdateRoute(ctx, "office", id, updated); err != nil {
		t.Fatal(err)
	}
	gotRoute, err := s.SelectedRoute(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if gotRoute != updated {
		t.Fatalf("updated route was not retained: got %+v, want %+v", gotRoute, updated)
	}
}

func TestJobRegistryRouteUpdateRejectsTerminalJob(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	id, err := s.Reserve(ctx, Job{Queue: "office", DocumentFormat: "image/urf"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminal(ctx, id, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRoute(ctx, "office", id, DocumentRoute{ClientDocumentFormat: "image/urf", UpstreamDocumentFormat: "image/pwg-raster", Kind: RouteURFToPWG}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal route update error = %v, want ErrInvalidTransition", err)
	}
}

func TestJobRegistryMigrationMakesLegacyUpstreamFieldsNullable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE jobs (
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
INSERT INTO jobs (proxy_job_id, upstream_job_id, upstream_job_uri, queue, requesting_user, document_format, state, created_at, updated_at)
VALUES (7, 91, 'ipp://legacy/jobs/91', 'legacy', 'bob', 'application/pdf', 'submitted', '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z');`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	legacy, err := s.GetByProxyID(context.Background(), "legacy", 7)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.UpstreamJobID != 91 || !legacy.HasUpstreamJobID || legacy.UpstreamJobURI != "ipp://legacy/jobs/91" || !legacy.HasUpstreamJobURI {
		t.Fatalf("legacy row was not preserved: %+v", legacy)
	}
	if legacy.UpstreamDocumentFormat != "application/pdf" || legacy.RouteKind != RoutePassThrough {
		t.Fatalf("legacy route was not defaulted to pass-through: %+v", legacy)
	}

	reservedID, err := s.Reserve(context.Background(), Job{Queue: "legacy", RequestingUser: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := s.GetByProxyID(context.Background(), "legacy", reservedID)
	if err != nil {
		t.Fatal(err)
	}
	if reserved.HasUpstreamJobID || reserved.HasUpstreamJobURI || reserved.State != StateReserved {
		t.Fatalf("new nullable reservation failed after migration: %+v", reserved)
	}

	var notNull int
	if err := s.db.QueryRow(`SELECT "notnull" FROM pragma_table_info('jobs') WHERE name = 'upstream_job_id'`).Scan(&notNull); err != nil {
		t.Fatal(err)
	}
	if notNull != 0 {
		t.Fatalf("upstream_job_id remains NOT NULL after migration")
	}
}

func TestJobRegistryMigrationBackfillsRoutesOnNullableLegacySchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
CREATE TABLE jobs (
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
);
INSERT INTO jobs (proxy_job_id, upstream_job_id, upstream_job_uri, queue, document_format, state, created_at, updated_at)
VALUES (8, 92, 'ipp://legacy/jobs/92', 'legacy', 'image/urf', 'mapped', '2025-01-02T03:04:05Z', '2025-01-02T03:04:05Z');`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.GetByProxyID(context.Background(), "legacy", 8)
	if err != nil {
		t.Fatal(err)
	}
	if job.UpstreamDocumentFormat != "image/urf" || job.RouteKind != RoutePassThrough {
		t.Fatalf("nullable legacy route was not backfilled: %+v", job)
	}
	var routeColumns int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name IN ('upstream_document_format','route_kind','route_sheet_back')`).Scan(&routeColumns); err != nil {
		t.Fatal(err)
	}
	if routeColumns != 3 {
		t.Fatalf("route columns = %d, want 3", routeColumns)
	}
}

func TestJobRegistryScopedMappingOwnershipAndFilters(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	first, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "alice", JobName: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "bob", JobName: "b"})
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.Reserve(ctx, Job{Queue: "lab", RequestingUser: "alice", JobName: "c"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMapped(ctx, first, 100, "ipp://printer/jobs/100"); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMapped(ctx, second, 100, "ipp://printer/jobs/100"); err == nil {
		t.Fatal("duplicate upstream mapping in one queue was accepted")
	}
	if err := s.MarkMapped(ctx, third, 100, "ipp://lab/jobs/100"); err != nil {
		t.Fatalf("same upstream id in another queue was rejected: %v", err)
	}

	officeAlice, err := s.ListJobs(ctx, JobFilter{Queue: "office", RequestingUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if len(officeAlice) != 1 || officeAlice[0].ProxyJobID != first {
		t.Fatalf("queue/user filter returned wrong jobs: %+v", officeAlice)
	}
	office, err := s.GetJobs(ctx, "office", JobFilter{States: []string{StateReserved, StateMapped}})
	if err != nil {
		t.Fatal(err)
	}
	if len(office) != 2 {
		t.Fatalf("state filter returned %d jobs, want 2: %+v", len(office), office)
	}
	myJobs, err := s.MyJobs(ctx, "office", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(myJobs) != 1 || myJobs[0].RequestingUser != "alice" {
		t.Fatalf("my-jobs lookup returned wrong jobs: %+v", myJobs)
	}
}

func TestJobRegistryDocumentsReconciliationAndRetention(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	id, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkMapped(ctx, id, 55, "ipp://printer/jobs/55"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDocument(ctx, id, "application/pdf", 100, intPtr(2), 1, intPtr(2), false); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDocument(ctx, id, "application/pdf", 50, intPtr(1), 1, intPtr(1), true); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if job.DocumentCount != 2 || !job.LastDocument || job.PayloadBytes != 150 || job.PageCount == nil || *job.PageCount != 3 {
		t.Fatalf("document accounting was not accumulated: %+v", job)
	}

	if err := s.MarkUncertain(ctx, id, "timeout"); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.ReconciliationCandidates(ctx, time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].ProxyJobID != id {
		t.Fatalf("uncertain job was not a reconciliation candidate: %+v", candidates)
	}

	old := time.Now().UTC().Add(-31 * 24 * time.Hour)
	oldID, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminalAt(ctx, oldID, "completed", "", old); err != nil {
		t.Fatal(err)
	}
	recentID, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "recent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminalAt(ctx, recentID, "completed", "", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	deleted, err := s.CleanupTerminalBefore(ctx, time.Now().UTC().Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("cleanup deleted %d jobs, want 1", deleted)
	}
	if _, err := s.GetByProxyID(ctx, "office", oldID); err == nil {
		t.Fatal("old terminal job was retained")
	}
	if _, err := s.GetByProxyID(ctx, "office", recentID); err != nil {
		t.Fatal("recent terminal job was deleted")
	}
	if _, err := s.GetByProxyID(ctx, "office", id); err != nil {
		t.Fatal("non-terminal uncertain job was deleted")
	}

	terminatorID, err := s.Reserve(ctx, Job{Queue: "office", RequestingUser: "terminator"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateDocument(ctx, terminatorID, "application/pdf", int64(0), nil, 1, nil, true); err != nil {
		t.Fatal(err)
	}
	terminator, err := s.GetByProxyID(ctx, "office", terminatorID)
	if err != nil {
		t.Fatal(err)
	}
	if terminator.DocumentCount != 0 || !terminator.LastDocument {
		t.Fatalf("no-data terminator counted as a document: %+v", terminator)
	}
	if err := s.MarkTerminal(ctx, "office", terminatorID, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateState(ctx, "office", terminatorID, StateMapped); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal row was resurrected, error=%v", err)
	}
}

func TestReconciliationAttemptsUseBoundedExponentialBackoff(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	id, err := s.Reserve(ctx, Job{Queue: "office", CreatedAt: base, UpdatedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUncertain(ctx, id, "response was lost", base); err != nil {
		t.Fatal(err)
	}

	firstAttempt := base.Add(time.Second)
	if err := s.MarkReconcileAttempt(ctx, id, firstAttempt); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if job.NextReconcileAt == nil || !job.NextReconcileAt.Equal(firstAttempt.Add(time.Minute)) {
		t.Fatalf("first reconciliation backoff = %v, want %v", job.NextReconcileAt, firstAttempt.Add(time.Minute))
	}

	secondAttempt := job.NextReconcileAt.Add(time.Second)
	if err := s.MarkReconcileAttempt(ctx, id, secondAttempt); err != nil {
		t.Fatal(err)
	}
	job, err = s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if job.NextReconcileAt == nil || !job.NextReconcileAt.Equal(secondAttempt.Add(2*time.Minute)) {
		t.Fatalf("second reconciliation backoff = %v, want %v", job.NextReconcileAt, secondAttempt.Add(2*time.Minute))
	}

	// Repeated misses must not grow without bound. Ten further attempts are
	// enough to reach the one-hour cap from the two-minute interval.
	for attempt := 0; attempt < 10; attempt++ {
		attemptAt := job.NextReconcileAt.Add(time.Second)
		if err := s.MarkReconcileAttempt(ctx, id, attemptAt); err != nil {
			t.Fatal(err)
		}
		job, err = s.GetByProxyID(ctx, "office", id)
		if err != nil {
			t.Fatal(err)
		}
	}
	if job.NextReconcileAt == nil || !job.NextReconcileAt.Equal(job.ReconcileAt.Add(time.Hour)) {
		t.Fatalf("capped reconciliation backoff = %v, want %v", job.NextReconcileAt, job.ReconcileAt.Add(time.Hour))
	}
}

func TestReconcileAttemptCannotRestoreTerminalClocks(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	id, err := s.Reserve(ctx, Job{Queue: "office", CreatedAt: base, UpdatedAt: base})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkUncertain(ctx, id, "response was lost", base); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminalAt(ctx, id, "completed", "", base.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkReconcileAttempt(ctx, id, base.Add(2*time.Minute)); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("MarkReconcileAttempt on terminal job = %v, want ErrInvalidTransition", err)
	}
	job, err := s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if !IsTerminalState(job.State) {
		t.Fatalf("terminal job state = %q, want terminal", job.State)
	}
	if job.ReconcileAt != nil || job.NextReconcileAt != nil {
		t.Fatalf("terminal job regained reconciliation clocks: reconcile=%v next=%v", job.ReconcileAt, job.NextReconcileAt)
	}
}

func TestReconciliationAttemptsDoNotStarveLaterCandidates(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	base := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	const total = 102
	ids := make([]int, 0, total)
	for index := 0; index < total; index++ {
		created := base.Add(time.Duration(index) * time.Second)
		id, err := s.Reserve(ctx, Job{Queue: "office", CreatedAt: created, UpdatedAt: created})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.MarkUncertain(ctx, id, "response was lost", base); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	initial, err := s.ReconciliationCandidates(ctx, base, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(initial) != 100 {
		t.Fatalf("initial candidate count = %d, want 100", len(initial))
	}
	for _, job := range initial {
		if err := s.MarkReconcileAttempt(ctx, job.ProxyJobID, base.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	// Before the first batch's one-minute retry window, the two rows that were
	// outside the batch must be eligible. This proves an oldest-100 batch cannot
	// remain continuously due and starve newer uncertain jobs.
	later, err := s.ReconciliationCandidates(ctx, base.Add(90*time.Second), 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(later) != 2 {
		t.Fatalf("later candidate count = %d, want 2: %+v", len(later), later)
	}
	want := map[int]struct{}{ids[100]: {}, ids[101]: {}}
	for _, job := range later {
		if _, ok := want[job.ProxyJobID]; !ok {
			t.Fatalf("later candidate %d was retried too early; want only IDs %d and %d", job.ProxyJobID, ids[100], ids[101])
		}
	}
}
