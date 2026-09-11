package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestStoreCreateAndLookupJob(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.CreateJob(context.Background(), Job{
		UpstreamJobID:        42,
		UpstreamJobURI:       "ipp://printer/jobs/42",
		Queue:                "office",
		RequestingUser:       "jnovak",
		JobName:              "invoice.pdf",
		DocumentFormat:       "application/pdf",
		State:                "created",
		PayloadBytes:         100,
		PageCount:            intPtr(2),
		Copies:               2,
		EstimatedImpressions: intPtr(4),
	})
	if err != nil {
		t.Fatal(err)
	}

	byProxy, err := s.GetByProxyID(context.Background(), "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if byProxy.UpstreamJobID != 42 || byProxy.UpstreamJobURI != "ipp://printer/jobs/42" || byProxy.RequestingUser != "jnovak" {
		t.Fatalf("unexpected proxy lookup: %+v", byProxy)
	}
	if byProxy.PayloadBytes != 100 || byProxy.PageCount == nil || *byProxy.PageCount != 2 || byProxy.Copies != 2 || byProxy.EstimatedImpressions == nil || *byProxy.EstimatedImpressions != 4 {
		t.Fatalf("unexpected metadata: %+v", byProxy)
	}

	byUpstream, err := s.GetByUpstreamID(context.Background(), "office", 42)
	if err != nil {
		t.Fatal(err)
	}
	if byUpstream.ProxyJobID != id {
		t.Fatalf("unexpected upstream lookup: %+v", byUpstream)
	}
}

func TestStoreSummarizeJobsAggregatesQueueAndLifecycleState(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	for _, job := range []Job{
		{Queue: "office", State: StateReserved},
		{Queue: "office", State: StateUncertain},
		{Queue: "home", State: StateTerminal},
	} {
		if _, err := s.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	summary, err := s.SummarizeJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Total != 3 {
		t.Fatalf("total jobs = %d, want 3", summary.Total)
	}
	if summary.ByQueue["office"] != 2 || summary.ByQueue["home"] != 1 {
		t.Fatalf("jobs by queue = %#v", summary.ByQueue)
	}
	if summary.ByState[StateReserved] != 1 || summary.ByState[StateUncertain] != 1 || summary.ByState[StateTerminal] != 1 {
		t.Fatalf("jobs by state = %#v", summary.ByState)
	}
}

func TestStoreUpdatePayloadMetadataAddsDocumentCounters(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	id, err := s.CreateJob(context.Background(), Job{
		UpstreamJobID: 42,
		Queue:         "office",
		State:         "created",
		Copies:        2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePayloadMetadata(context.Background(), "office", id, "application/pdf", 50, intPtr(3), 2, intPtr(6)); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdatePayloadMetadata(context.Background(), "office", id, "application/pdf", 25, intPtr(1), 2, intPtr(2)); err != nil {
		t.Fatal(err)
	}

	job, err := s.GetByProxyID(context.Background(), "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if job.PayloadBytes != 75 || job.PageCount == nil || *job.PageCount != 4 || job.EstimatedImpressions == nil || *job.EstimatedImpressions != 8 {
		t.Fatalf("metadata was not accumulated: %+v", job)
	}
}

func TestStoreRepeatedTerminalObservationsPreserveFirstTerminalAt(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	id, err := s.Reserve(ctx, Job{Queue: "office"})
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	second := first.Add(time.Hour)

	if err := s.MarkTerminalAt(ctx, id, "completed", "", first); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkTerminalAt(ctx, id, "failed", "later failure", second); err != nil {
		t.Fatal(err)
	}

	job, err := s.GetByProxyID(ctx, "office", id)
	if err != nil {
		t.Fatal(err)
	}
	if job.TerminalAt == nil || !job.TerminalAt.Equal(first) {
		t.Fatalf("repeated terminal observation replaced first terminal_at: got %v, want %v", job.TerminalAt, first)
	}
	if job.ObservedState != "failed" || job.ObservedError != "later failure" {
		t.Fatalf("latest terminal observation was not retained: %+v", job)
	}
}

func TestStoreConcurrentObservedUpdateCannotOverwriteTerminal(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	const iterations = 100
	for iteration := 0; iteration < iterations; iteration++ {
		id, err := s.Reserve(ctx, Job{Queue: "office"})
		if err != nil {
			t.Fatal(err)
		}

		start := make(chan struct{})
		var group sync.WaitGroup
		var observedErr, terminalErr error
		group.Add(2)
		go func() {
			defer group.Done()
			<-start
			observedErr = s.UpdateObserved(ctx, id, "processing", "stale observation")
		}()
		go func() {
			defer group.Done()
			<-start
			terminalErr = s.MarkTerminalAt(ctx, id, "completed", "", time.Unix(int64(iteration+1), 0).UTC())
		}()
		close(start)
		group.Wait()

		if terminalErr != nil {
			t.Fatalf("iteration %d: MarkTerminal failed: %v", iteration, terminalErr)
		}
		if observedErr != nil && !errors.Is(observedErr, ErrInvalidTransition) {
			t.Fatalf("iteration %d: unexpected UpdateObserved error: %v", iteration, observedErr)
		}

		job, err := s.GetByProxyID(ctx, "office", id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != StateTerminal || job.ObservedState != "completed" || job.ObservedError != "" {
			t.Fatalf("iteration %d: nonterminal observation overwrote terminal result: %+v", iteration, job)
		}
	}
}

func intPtr(v int) *int {
	return &v
}
