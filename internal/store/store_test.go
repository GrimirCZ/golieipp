package store

import (
	"context"
	"testing"
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

func intPtr(v int) *int {
	return &v
}
