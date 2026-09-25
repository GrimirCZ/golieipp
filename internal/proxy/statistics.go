package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/OpenPrinting/goipp"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/stats"
	"github.com/grimir/golieipp/internal/store"
)

// SetStatistics is called before serving requests or starting managed loops.
func (s *Service) SetStatistics(c *stats.Collector, registryID string) {
	s.statistics = c
	s.statisticsID = registryID
	s.upstream.statistics = c
}

func (s *Service) statsRequest(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request, func()) {
	ctx, span := stats.Begin(r.Context(), s.statistics, stats.Action{Kind: "client-request", Operation: "unknown", Queue: queueFromPath(r.URL.Path), Outcome: "error"})
	if span == nil {
		return w, r, func() {}
	}
	span.SetConcurrency(s.statsActive.Add(1))
	return &statisticsResponseWriter{ResponseWriter: w, span: span}, r.WithContext(ctx), func() {
		s.statsActive.Add(-1)
		if r.Context().Err() != nil {
			span.SetReason("client-context-ended")
		}
		span.Finish("")
	}
}

func (s *Service) statsUser(ctx context.Context, user string) func() {
	span := stats.SpanFromContext(ctx)
	if span == nil {
		return func() {}
	}
	span.SetUser(user)
	s.statsUserMu.Lock()
	if s.statsUsers == nil {
		s.statsUsers = make(map[string]int64)
	}
	s.statsUsers[user]++
	n := s.statsUsers[user]
	s.statsUserMu.Unlock()
	span.SetUserConcurrency(n)
	return func() {
		s.statsUserMu.Lock()
		s.statsUsers[user]--
		if s.statsUsers[user] == 0 {
			delete(s.statsUsers, user)
		}
		s.statsUserMu.Unlock()
	}
}

type statisticsResponseWriter struct {
	http.ResponseWriter
	span *stats.Span
}

func (w *statisticsResponseWriter) WriteHeader(n int) {
	w.span.SetHTTPStatus(n)
	if n >= 400 {
		w.span.SetOutcome("rejected")
	}
	w.ResponseWriter.WriteHeader(n)
}
func (w *statisticsResponseWriter) Write(b []byte) (int, error) {
	n, e := w.ResponseWriter.Write(b)
	if e != nil {
		w.span.SetOutcome("error")
		w.span.SetReason("response-write")
	}
	return n, e
}
func (w *statisticsResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *statisticsResponseWriter) recordIPP(m *goipp.Message) {
	w.span.SetIPPStatus(int(m.Code))
	w.span.SetReason(goipp.Status(m.Code).String())
	if ippSuccess(m) {
		w.span.SetOutcome("success")
	} else {
		w.span.SetOutcome("rejected")
	}
}

type statisticsReader struct {
	io.Reader
	span     *stats.Span
	upstream bool
}

func (r *statisticsReader) Read(p []byte) (int, error) {
	n, e := r.Reader.Read(p)
	if r.upstream {
		r.span.AddUpstreamBytes(int64(n))
	} else {
		r.span.AddClientBytes(int64(n))
	}
	return n, e
}

func (s *Service) statsSnapshot(queue string, id int, register bool, requested, effective goipp.Attributes) *stats.Job {
	if !s.statistics.Enabled() {
		return nil
	}
	ctx, cancel := s.persistenceContext()
	defer cancel()
	j, err := s.store.GetByProxyID(ctx, queue, id)
	if err != nil {
		return nil
	}
	x := statsJob(j, s.statisticsID)
	x.Register = register
	if register {
		x.RequestedColor, _ = iattr.FirstString(requested, "print-color-mode")
		x.EffectiveColor, _ = iattr.FirstString(effective, "print-color-mode")
		x.RequestedSides, _ = iattr.FirstString(requested, "sides")
		x.EffectiveSides, _ = iattr.FirstString(effective, "sides")
		x.RequestedMedia, _ = iattr.FirstString(requested, "media")
		x.EffectiveMedia, _ = iattr.FirstString(effective, "media")
		if x.EffectiveMedia == "" {
			x.EffectiveMedia = j.RouteMediaName
		}
		if x.EffectiveSides == "" {
			x.EffectiveSides = j.RouteSides
		}
	}
	return &x
}
func statsJob(j store.Job, registry string) stats.Job {
	x := stats.Job{RegistryID: registry, JobID: j.ProxyJobID, Queue: j.Queue, User: j.RequestingUser, CreatedAt: j.CreatedAt, UpdatedAt: time.Now().UTC(), State: j.State, Accepted: j.HasUpstreamJobID || j.HasUpstreamJobURI, DocumentCount: j.DocumentCount, Copies: j.Copies, PayloadBytes: j.PayloadBytes, PageCount: j.PageCount, EstimatedImpressions: j.EstimatedImpressions, DocumentFormat: j.DocumentFormat, UpstreamFormat: j.UpstreamDocumentFormat, Route: j.RouteKind}
	// Primary cancellation state records request acceptance, not a printer
	// observation. Only genuine response attributes set ObservedState below.
	if j.State == store.StateFailed {
		x.TerminalAt = j.TerminalAt
	}
	return x
}
func (s *Service) statsRegister(ctx context.Context, queue string, id int, requested, effective goipp.Attributes) {
	if x := s.statsSnapshot(queue, id, true, requested, effective); x != nil {
		s.statistics.ObserveJob(*x)
		if span := stats.SpanFromContext(ctx); span != nil {
			span.SetJob(s.statisticsID, id)
			span.SetOwner(x.User)
		}
	}
}
func (s *Service) statsStored(queue string, id int) {
	if x := s.statsSnapshot(queue, id, false, nil, nil); x != nil {
		s.statistics.ObserveJob(*x)
	}
}
func (s *Service) statsResponse(queue string, id int, attrs goipp.Attributes, accepted, cancelAccepted bool) {
	x := s.statsSnapshot(queue, id, false, nil, nil)
	if x == nil {
		return
	}
	x.Accepted = x.Accepted || accepted
	x.CancelAccepted = cancelAccepted
	applyStatsObservation(x, attrs)
	s.statistics.ObserveJob(*x)
}
func applyStatsObservation(x *stats.Job, attrs goipp.Attributes) {
	if n, ok := iattr.FirstInt(attrs, "job-state"); ok {
		x.ObservedState = map[int]string{3: "pending", 4: "pending-held", 5: "processing", 6: "processing-stopped", 7: "canceled", 8: "aborted", 9: "completed"}[n]
		now := time.Now().UTC()
		if n == 5 || n == 6 {
			x.ProcessingAt = &now
		}
		if n >= 7 && n <= 9 {
			x.TerminalAt = &now
		}
	}
	if reason, ok := iattr.FirstString(attrs, "job-state-reasons"); ok {
		x.StateReasons = reason
	}
	if n, ok := iattr.FirstInt(attrs, "job-impressions-completed"); ok && n >= 0 {
		x.CompletedImpressions = &n
	}
	if n, ok := iattr.FirstInt(attrs, "job-media-sheets-completed"); ok && n >= 0 {
		x.CompletedSheets = &n
	}
}

func outcomeForError(err error) string {
	if err != nil {
		return "error"
	}
	return "success"
}

func (s *Service) statsBackground(ctx context.Context, kind, operation, queue string) (context.Context, *stats.Span) {
	return stats.Begin(ctx, s.statistics, stats.Action{Kind: kind, Operation: operation, Queue: queue})
}

// Known reasons are bounded protocol keywords; arbitrary upstream text and
// document/job names are deliberately not copied to the statistics database.
func statisticsReason(err error) string {
	if err == nil {
		return ""
	}
	if strings.Contains(err.Error(), "context") {
		return "context-ended"
	}
	return "operation-failed"
}
