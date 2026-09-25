package proxy

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/stats"
)

func attachTestStatistics(t *testing.T, s *Service) (*stats.Collector, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.sqlite")
	c := stats.New(stats.Options{Enabled: true, Path: path, QueueSize: 1024, BatchSize: 128, FlushInterval: time.Millisecond, Resources: true, CPU: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	id, e := s.store.InstanceID(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	s.SetStatistics(c, id)
	t.Cleanup(func() { _ = c.Close() })
	return c, path
}
func queryTestStatistics(t *testing.T, path, command string) stats.Report {
	t.Helper()
	r, e := stats.Query(context.Background(), path, stats.ReportOptions{Command: command, Limit: 1000})
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func statsIPP(t *testing.T, s *Service, op goipp.Op, id int, doc []byte, format string, unknownLength bool, last bool) *goipp.Message {
	t.Helper()
	m := goipp.NewRequest(goipp.DefaultVersion, op, 1)
	m.Operation = acceptanceBasicOperationAttrs("ipp://proxy.example:631/printers/office")
	if id > 0 {
		m.Operation = append(m.Operation, iattr.Integer("job-id", id))
	}
	m.Operation = append(m.Operation, iattr.Name("requesting-user-name", "alice"))
	if format != "" {
		m.Operation = append(m.Operation, goipp.MakeAttribute("document-format", goipp.TagMimeType, goipp.String(format)))
	}
	if op == goipp.OpSendDocument {
		m.Operation = append(m.Operation, goipp.MakeAttribute("last-document", goipp.TagBoolean, goipp.Boolean(last)))
	}
	envelope, e := m.EncodeBytes()
	if e != nil {
		t.Fatal(e)
	}
	r := httptest.NewRequest(http.MethodPost, "/printers/office", io.MultiReader(bytes.NewReader(envelope), bytes.NewReader(doc)))
	r.Header.Set("Content-Type", goipp.ContentType)
	r.ContentLength = int64(len(envelope) + len(doc))
	if unknownLength {
		r.ContentLength = -1
	}
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	out := &goipp.Message{}
	if e := out.Decode(w.Body); e != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), e)
	}
	return out
}

func TestStatisticsEndToEndJobsAndCancellation(t *testing.T) {
	u := newAcceptanceUpstream(t, nil)
	s := newAcceptanceService(t, u, config.AirPrintAuto, "monochrome")
	defer s.Close()
	activateTestQueue(s, "office")
	c, path := attachTestStatistics(t, s)
	doc := []byte("%PDF-1.7\n/Type /Page\n%%EOF")
	first := statsIPP(t, s, goipp.OpPrintJob, 0, doc, "application/pdf", true, false)
	if !ippSuccess(first) {
		t.Fatal(statusMessage(first))
	}
	one, _ := iattr.FirstInt(first.Job, "job-id")
	created := statsIPP(t, s, goipp.OpCreateJob, 0, nil, "application/pdf", false, false)
	if !ippSuccess(created) {
		t.Fatal(statusMessage(created))
	}
	two, _ := iattr.FirstInt(created.Job, "job-id")
	if r := statsIPP(t, s, goipp.OpSendDocument, two, doc, "application/pdf", false, false); !ippSuccess(r) {
		t.Fatal(statusMessage(r))
	}
	if r := statsIPP(t, s, goipp.OpSendDocument, two, nil, "", false, true); !ippSuccess(r) {
		t.Fatal(statusMessage(r))
	}
	if r := statsIPP(t, s, goipp.OpCancelJob, one, nil, "", false, false); !ippSuccess(r) {
		t.Fatal(statusMessage(r))
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	r := queryTestStatistics(t, path, "summary")
	row := r.Rows[0]
	if row["job_count"] != int64(2) || row["document_count"] != int64(2) || row["payload_bytes"] != int64(2*len(doc)) || row["cancel_accepted_count"] != int64(1) || row["canceled_count"] != int64(0) || row["completed_impressions"] != nil {
		t.Fatalf("accounting: %+v", row)
	}
	jobs := queryTestStatistics(t, path, "jobs")
	for _, j := range jobs.Rows {
		if j["observed_state"] != "" {
			t.Fatalf("synthetic response state recorded: %+v", j)
		}
	}
	actions := queryTestStatistics(t, path, "actions")
	var clientBytes, upstreamBytes int64
	for _, a := range actions.Rows {
		if a["kind"] == "client-request" {
			clientBytes += a["client_bytes"].(int64)
		}
		if a["kind"] == "upstream-request" {
			upstreamBytes += a["upstream_bytes"].(int64)
		}
	}
	if clientBytes != int64(2*len(doc)) || upstreamBytes != clientBytes {
		t.Fatalf("payload counted twice: client=%d upstream=%d", clientBytes, upstreamBytes)
	}
	// Restarting recording retains historical identity and updates registered
	// jobs without backfilling unrelated primary registry rows.
	next := stats.New(stats.Options{Enabled: true, Path: path, QueueSize: 32, BatchSize: 16, FlushInterval: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.SetStatistics(next, s.statisticsID)
	j, e := s.store.GetByProxyID(context.Background(), "office", one)
	if e != nil {
		t.Fatal(e)
	}
	attrs := goipp.Attributes{goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(7)), iattr.Integer("job-impressions-completed", 2), iattr.Integer("job-media-sheets-completed", 1)}
	s.syncObservedJob(context.Background(), "office", j, attrs)
	s.syncObservedJob(context.Background(), "office", j, attrs)
	if e := next.Close(); e != nil {
		t.Fatal(e)
	}
	r = queryTestStatistics(t, path, "summary")
	if r.Rows[0]["job_count"] != int64(2) || r.Rows[0]["canceled_count"] != int64(1) || r.Rows[0]["completed_impressions"] != int64(2) {
		t.Fatalf("late repeated observation: %+v", r.Rows)
	}
}

func TestStatisticsTranslationTracksHeldFilesAndFailures(t *testing.T) {
	u := newAcceptanceUpstream(t, acceptanceRasterCapabilities("monochrome", false, true))
	s := newAcceptanceService(t, u, config.AirPrintEmulateIfMissing, "monochrome")
	defer s.Close()
	c, path := attachTestStatistics(t, s)
	if e := s.RefreshAll(context.Background()); e != nil {
		t.Fatal(e)
	}
	doc := acceptanceURF(595, 841, 72, 0, 8)
	reply := statsIPP(t, s, goipp.OpPrintJob, 0, doc, "image/urf", true, false)
	if !ippSuccess(reply) {
		t.Fatal(statusMessage(reply))
	}
	forwarded := u.lastOperation(goipp.OpPrintJob).payload
	bad := statsIPP(t, s, goipp.OpPrintJob, 0, []byte("bad URF payload"), "image/urf", false, false)
	if ippSuccess(bad) {
		t.Fatal("malformed translation accepted")
	}
	if e := c.Close(); e != nil {
		t.Fatal(e)
	}
	r := queryTestStatistics(t, path, "actions")
	var translationOK, translationFailed bool
	var measured bool
	for _, a := range r.Rows {
		if a["kind"] == "translation" {
			if a["outcome"] == "success" {
				translationOK = true
				if a["output_bytes"] != int64(len(forwarded)) {
					t.Fatalf("output size: %+v", a)
				}
			} else {
				translationFailed = true
			}
		}
		if a["kind"] == "client-request" && a["outcome"] == "success" {
			measured = true
			if a["temp_peak_bytes"].(int64) < int64(len(doc)+len(forwarded)) {
				t.Fatalf("overlapping staged files undercounted: %+v", a)
			}
			if a["buffer_peak_bytes"].(int64) > 1024*1024 {
				t.Fatalf("logical raster reported as memory: %+v", a)
			}
		}
	}
	if !translationOK || !translationFailed || !measured {
		t.Fatalf("missing action coverage: %+v", r.Rows)
	}
	summary := queryTestStatistics(t, path, "summary")
	if summary.Rows[0]["job_count"] != int64(1) {
		t.Fatal("failed pre-reservation translation became a job")
	}
}

type discardStatsRecorder struct{}

func (discardStatsRecorder) RecordAction(stats.Action) {}

type statisticsZeros struct{}

func (statisticsZeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func BenchmarkStatisticsStagingMemory(b *testing.B) {
	for _, size := range []int64{64 * 1024, 4 * 1024 * 1024} {
		name := "64KiB"
		if size > 64*1024 {
			name = "4MiB"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(size)
			for b.Loop() {
				ctx, span := stats.Begin(context.Background(), discardStatsRecorder{}, stats.Action{Kind: "client-request"})
				body := io.NopCloser(io.LimitReader(statisticsZeros{}, size))
				staged, err := stageUnknownPayload(ctx, body, body, size)
				if err != nil {
					b.Fatal(err)
				}
				if err = staged.Close(); err != nil {
					b.Fatal(err)
				}
				span.Finish("success")
			}
		})
	}
}
