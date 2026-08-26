package proxy

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

type Service struct {
	cfg      *config.Config
	store    *store.Store
	upstream *UpstreamClient
	logger   *slog.Logger
	nextID   atomic.Uint64

	mu           sync.RWMutex
	capabilities map[string]goipp.Attributes
}

type traceIDContextKey struct{}

func NewService(cfg *config.Config, jobStore *store.Store, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg:          cfg,
		store:        jobStore,
		upstream:     NewUpstreamClient(logger),
		logger:       logger,
		capabilities: map[string]goipp.Attributes{},
	}, nil
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/printers/", s.ippHandler)
	mux.HandleFunc("/ipp/", s.ippHandler)
	return s.logHTTP(mux)
}

func (s *Service) logHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceID := s.nextID.Add(1)
		start := time.Now()
		logger := s.logger.With(
			"trace_id", traceID,
			"method", r.Method,
			"path", r.URL.Path,
			"remote_addr", r.RemoteAddr,
			"user_agent", r.UserAgent(),
		)
		logger.Info("http request received",
			"content_type", r.Header.Get("content-type"),
			"content_length", r.ContentLength,
		)

		rec := &loggingResponseWriter{ResponseWriter: w, status: http.StatusOK}
		ctx := context.WithValue(r.Context(), traceIDContextKey{}, traceID)
		next.ServeHTTP(rec, r.WithContext(ctx))

		logger.Info("http response sent",
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", durationMillis(start),
		)
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
	wrote  bool
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.bytes += int64(n)
	return n, err
}

func (s *Service) RefreshAll(ctx context.Context) error {
	for queue, printer := range s.cfg.Printers {
		if printer.Optional {
			continue
		}
		if err := s.refreshOne(ctx, queue, printer); err != nil {
			return fmt.Errorf("%s: %w", queue, err)
		}
	}
	return nil
}

func (s *Service) StartRefreshLoop(ctx context.Context) {
	timers := make([]*time.Ticker, 0, len(s.cfg.Printers))
	defer func() {
		for _, timer := range timers {
			timer.Stop()
		}
	}()
	for queue, printer := range s.cfg.Printers {
		q := queue
		p := printer
		interval := p.RefreshInterval
		if interval == 0 {
			interval = 5 * time.Minute
		}
		ticker := time.NewTicker(interval)
		timers = append(timers, ticker)
		go func() {
			if !s.printerAvailable(q) {
				s.refreshInBackground(ctx, q, p)
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					s.refreshInBackground(ctx, q, p)
				}
			}
		}()
	}
	<-ctx.Done()
}

func (s *Service) LogPrinterURLs() {
	for queue, printer := range s.cfg.Printers {
		if s.printerAvailable(queue) {
			s.logPrinterAvailable(queue, printer)
		}
	}
}

func (s *Service) refreshInBackground(ctx context.Context, queue string, printer config.PrinterConfig) {
	wasAvailable := s.printerAvailable(queue)
	if err := s.refreshOne(ctx, queue, printer); err != nil {
		s.logger.Warn("refresh upstream capabilities failed", "queue", queue, "error", err)
		return
	}
	if !wasAvailable {
		s.logPrinterAvailable(queue, printer)
	}
}

func (s *Service) printerAvailable(queue string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.capabilities[queue]
	return ok
}

func (s *Service) logPrinterAvailable(queue string, printer config.PrinterConfig) {
	s.logger.Info("printer available",
		"queue", queue,
		"display_name", printer.DisplayName,
		"printer_url", s.proxyPrinterURI(queue),
	)
}

func (s *Service) refreshOne(ctx context.Context, queue string, printer config.PrinterConfig) error {
	start := time.Now()
	s.logger.Debug("refresh upstream capabilities started",
		"queue", queue,
		"upstream_uri", printer.UpstreamURI,
	)
	resp, err := s.upstream.GetPrinterAttributes(ctx, printer.UpstreamURI)
	if err != nil {
		s.logger.Debug("refresh upstream capabilities request failed",
			"queue", queue,
			"upstream_uri", printer.UpstreamURI,
			"duration_ms", durationMillis(start),
			"error", err,
		)
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusErrorBadRequest {
		s.logger.Debug("refresh upstream capabilities rejected",
			"queue", queue,
			"upstream_uri", printer.UpstreamURI,
			"duration_ms", durationMillis(start),
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
		)
		return fmt.Errorf("upstream returned %s", goipp.Status(resp.Code))
	}
	if err := ValidatePolicyAgainstUpstream(resp.Printer, printer); err != nil {
		s.logger.Debug("refresh upstream capabilities validation failed",
			"queue", queue,
			"upstream_uri", printer.UpstreamURI,
			"duration_ms", durationMillis(start),
			"error", err,
		)
		return err
	}
	s.mu.Lock()
	s.capabilities[queue] = resp.Printer.DeepCopy()
	s.mu.Unlock()
	s.logger.Debug("refresh upstream capabilities completed",
		"queue", queue,
		"upstream_uri", printer.UpstreamURI,
		"duration_ms", durationMillis(start),
		"printer_attr_count", len(resp.Printer),
	)
	return nil
}

func (s *Service) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) ippHandler(w http.ResponseWriter, r *http.Request) {
	traceID, _ := r.Context().Value(traceIDContextKey{}).(uint64)
	if traceID == 0 {
		traceID = s.nextID.Add(1)
	}
	start := time.Now()
	logger := s.logger.With(
		"trace_id", traceID,
		"method", r.Method,
		"path", r.URL.Path,
		"remote_addr", r.RemoteAddr,
		"user_agent", r.UserAgent(),
	)
	logger.Info("ipp http request received",
		"content_type", r.Header.Get("content-type"),
		"content_length", r.ContentLength,
	)
	if r.Method != http.MethodPost {
		logger.Warn("ipp http request rejected", "reason", "method_not_allowed")
		http.Error(w, "IPP requires POST", http.StatusMethodNotAllowed)
		return
	}
	queue := queueFromPath(r.URL.Path)
	logger = logger.With("queue", queue)
	printer, ok := s.cfg.Printers[queue]
	if !ok {
		logger.Warn("ipp request rejected", "reason", "unknown_printer")
		s.writeIPPError(w, goipp.DefaultVersion, 1, goipp.StatusErrorNotFound, "unknown printer")
		return
	}

	br := bufio.NewReader(r.Body)
	req := &goipp.Message{}
	if err := req.Decode(br); err != nil {
		logger.Warn("ipp request decode failed",
			"duration_ms", durationMillis(start),
			"error", err,
		)
		s.writeIPPError(w, goipp.DefaultVersion, 1, goipp.StatusErrorBadRequest, err.Error())
		return
	}
	op := goipp.Op(req.Code)
	logger = logger.With(
		"ipp_version", req.Version.String(),
		"ipp_request_id", req.RequestID,
		"operation", op.String(),
	)
	logger.Debug("ipp request decoded", requestLogAttrs(req)...)

	var (
		resp *goipp.Message
		err  error
	)
	switch op {
	case goipp.OpGetPrinterAttributes:
		resp, err = s.handleGetPrinterAttributes(r.Context(), queue, printer, req)
	case goipp.OpValidateJob:
		resp, err = s.handleValidateJob(r.Context(), queue, printer, req)
	case goipp.OpPrintJob:
		resp, err = s.handlePrintJob(r.Context(), queue, printer, req, br)
	case goipp.OpCreateJob:
		resp, err = s.handleCreateJob(r.Context(), queue, printer, req)
	case goipp.OpSendDocument:
		resp, err = s.handleSendDocument(r.Context(), queue, printer, req, br)
	case goipp.OpGetJobAttributes, goipp.OpCancelJob, goipp.OpCloseJob:
		resp, err = s.handleMappedJobOperation(r.Context(), queue, printer, req)
	case goipp.OpGetJobs:
		resp, err = s.forwardPrinterOperation(r.Context(), printer, req)
	default:
		resp = goipp.NewResponse(req.Version, goipp.StatusErrorOperationNotSupported, req.RequestID)
		resp.Operation = responseOperationAttrs("unsupported operation")
	}
	if err != nil {
		logger.Error("ipp operation failed",
			"duration_ms", durationMillis(start),
			"error", err,
		)
		resp = goipp.NewResponse(req.Version, goipp.StatusErrorServiceUnavailable, req.RequestID)
		resp.Operation = responseOperationAttrs(err.Error())
	}
	logger.Debug("ipp response prepared",
		"duration_ms", durationMillis(start),
		"ipp_status", goipp.Status(resp.Code).String(),
		"status_message", statusMessage(resp),
		"operation_attr_count", len(resp.Operation),
		"printer_attr_count", len(resp.Printer),
		"job_attr_count", len(resp.Job),
	)
	writeIPP(w, resp)
}

func (s *Service) handleGetPrinterAttributes(_ context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	s.mu.RLock()
	upstream, ok := s.capabilities[queue]
	s.mu.RUnlock()
	if !ok {
		return goipp.NewResponse(req.Version, goipp.StatusErrorPrinterIsDeactivated, req.RequestID), nil
	}
	resp := goipp.NewResponse(req.Version, goipp.StatusOk, req.RequestID)
	resp.Operation = responseOperationAttrs("")
	resp.Printer = FilterPrinterAttributes(upstream.DeepCopy(), queue, s.proxyPrinterURI(queue), printer)
	return resp, nil
}

func (s *Service) handleValidateJob(ctx context.Context, _ string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	normalized, _ := NormalizeJobAttrs(req.Job, printer.Policy, printer.Passthrough.DropVendorAttrs)
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized
	req.Groups = nil
	return s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
}

func (s *Service) handlePrintJob(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message, payload *bufio.Reader) (*goipp.Message, error) {
	normalized, normLog := NormalizeJobAttrs(req.Job, printer.Policy, printer.Passthrough.DropVendorAttrs)
	user, _ := iattr.FirstString(req.Operation, "requesting-user-name")
	jobName, _ := iattr.FirstString(req.Operation, "job-name")
	format, _ := iattr.FirstString(req.Operation, "document-format")
	copies := copiesFromAttrs(normalized)
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized
	req.Groups = nil

	recorder, err := newPayloadRecorder(payload)
	if err != nil {
		return nil, err
	}
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, recorder.Reader())
	meta, metaErr := recorder.Finish(format, copies)
	if metaErr != nil {
		s.logger.Warn("payload metadata extraction failed", "queue", queue, "job_name", jobName, "error", metaErr)
	}
	if err != nil {
		return nil, err
	}
	if !ippSuccess(resp) {
		s.logger.Warn("print job rejected by upstream",
			"queue", queue,
			"user", user,
			"job_name", jobName,
			"document_format", format,
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
			"payload_bytes", meta.Bytes,
		)
		return resp, nil
	}
	upstreamJobID, _ := iattr.FirstInt(resp.Job, "job-id")
	if upstreamJobID > 0 {
		upstreamJobURI, _ := iattr.FirstString(resp.Job, "job-uri")
		proxyID, err := s.store.CreateJob(ctx, store.Job{
			UpstreamJobID:        upstreamJobID,
			UpstreamJobURI:       upstreamJobURI,
			Queue:                queue,
			RequestingUser:       user,
			JobName:              jobName,
			DocumentFormat:       format,
			State:                "submitted",
			PayloadBytes:         meta.Bytes,
			PageCount:            meta.PageCount,
			Copies:               meta.Copies,
			EstimatedImpressions: meta.EstimatedImpressions,
		})
		if err != nil {
			return nil, err
		}
		rewriteResponseJob(resp, queue, proxyID, s.proxyPrinterURI(queue))
		attrs := normLog.Attrs()
		args := []any{
			"queue", queue,
			"user", user,
			"job_name", jobName,
			"document_format", format,
			"upstream_job_id", upstreamJobID,
			"proxy_job_id", proxyID,
			"payload_bytes", meta.Bytes,
			"page_count", nullableLogInt(meta.PageCount),
			"copies", meta.Copies,
			"estimated_impressions", nullableLogInt(meta.EstimatedImpressions),
		}
		for _, attr := range attrs {
			args = append(args, attr.Key, attr.Value.Any())
		}
		s.logger.Info("job submitted", args...)
	}
	return resp, nil
}

func (s *Service) handleCreateJob(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	normalized, normLog := NormalizeJobAttrs(req.Job, printer.Policy, printer.Passthrough.DropVendorAttrs)
	user, _ := iattr.FirstString(req.Operation, "requesting-user-name")
	jobName, _ := iattr.FirstString(req.Operation, "job-name")
	format, _ := iattr.FirstString(req.Operation, "document-format")
	copies := copiesFromAttrs(normalized)
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized
	req.Groups = nil

	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	if err != nil {
		return nil, err
	}
	if !ippSuccess(resp) {
		attrs := normLog.Attrs()
		args := []any{
			"queue", queue,
			"user", user,
			"job_name", jobName,
			"document_format", format,
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
			"copies", copies,
		}
		for _, attr := range attrs {
			args = append(args, attr.Key, attr.Value.Any())
		}
		s.logger.Warn("create job rejected by upstream", args...)
		return resp, nil
	}
	upstreamJobID, _ := iattr.FirstInt(resp.Job, "job-id")
	if upstreamJobID > 0 {
		upstreamJobURI, _ := iattr.FirstString(resp.Job, "job-uri")
		proxyID, err := s.store.CreateJob(ctx, store.Job{
			UpstreamJobID:  upstreamJobID,
			UpstreamJobURI: upstreamJobURI,
			Queue:          queue,
			RequestingUser: user,
			JobName:        jobName,
			DocumentFormat: format,
			State:          "created",
			Copies:         copies,
		})
		if err != nil {
			return nil, err
		}
		rewriteResponseJob(resp, queue, proxyID, s.proxyPrinterURI(queue))
		attrs := normLog.Attrs()
		args := []any{"queue", queue, "user", user, "job_name", jobName, "document_format", format, "upstream_job_id", upstreamJobID, "proxy_job_id", proxyID, "copies", copies}
		for _, attr := range attrs {
			args = append(args, attr.Key, attr.Value.Any())
		}
		s.logger.Info("job created", args...)
	}
	return resp, nil
}

func (s *Service) handleSendDocument(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message, payload *bufio.Reader) (*goipp.Message, error) {
	proxyJobID := requestProxyJobID(req)
	job, err := s.lookupRequestJob(ctx, queue, req)
	if err != nil {
		return nil, err
	}
	if proxyJobID == 0 {
		proxyJobID = job.ProxyJobID
	}
	format, _ := iattr.FirstString(req.Operation, "document-format")
	if format == "" {
		format = job.DocumentFormat
	}
	req.Operation = rewriteMappedJobOperation(req.Operation, job)
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Groups = nil

	recorder, err := newPayloadRecorder(payload)
	if err != nil {
		return nil, err
	}
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, recorder.Reader())
	meta, metaErr := recorder.Finish(format, job.Copies)
	if metaErr != nil {
		s.logger.Warn("payload metadata extraction failed", "queue", queue, "proxy_job_id", proxyJobID, "error", metaErr)
	}
	if err != nil {
		return nil, err
	}
	if !ippSuccess(resp) {
		s.logger.Warn("send document rejected by upstream",
			"queue", queue,
			"proxy_job_id", proxyJobID,
			"upstream_job_id", job.UpstreamJobID,
			"document_format", format,
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
			"payload_bytes", meta.Bytes,
		)
		return resp, nil
	}
	if proxyJobID > 0 {
		if err := s.store.UpdatePayloadMetadata(ctx, queue, proxyJobID, format, meta.Bytes, meta.PageCount, meta.Copies, meta.EstimatedImpressions); err != nil {
			return nil, err
		}
		s.logger.Info("job document submitted",
			"queue", queue,
			"proxy_job_id", proxyJobID,
			"upstream_job_id", job.UpstreamJobID,
			"document_format", format,
			"payload_bytes", meta.Bytes,
			"page_count", nullableLogInt(meta.PageCount),
			"copies", meta.Copies,
			"estimated_impressions", nullableLogInt(meta.EstimatedImpressions),
		)
	}
	return resp, nil
}

func (s *Service) handleMappedJobOperation(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	if err := s.rewriteRequestJobID(ctx, queue, printer, req); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID)
			resp.Operation = responseOperationAttrs("unknown job")
			return resp, nil
		}
		return nil, err
	}
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Groups = nil
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	if err != nil {
		return nil, err
	}
	if !ippSuccess(resp) {
		s.logger.Warn("mapped job operation rejected by upstream",
			"queue", queue,
			"operation", goipp.Op(req.Code).String(),
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
		)
		return resp, nil
	}
	if id, ok := iattr.FirstInt(resp.Job, "job-id"); ok {
		if job, err := s.store.GetByUpstreamID(ctx, queue, id); err == nil {
			rewriteResponseJob(resp, queue, job.ProxyJobID, s.proxyPrinterURI(queue))
		}
	}
	return resp, nil
}

func (s *Service) forwardPrinterOperation(ctx context.Context, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Groups = nil
	return s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
}

func (s *Service) rewriteRequestJobID(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) error {
	proxyJobID := requestProxyJobID(req)
	if proxyJobID == 0 {
		return nil
	}
	job, err := s.store.GetByProxyID(ctx, queue, proxyJobID)
	if err != nil {
		return err
	}
	req.Operation = rewriteMappedJobOperation(req.Operation, job)
	return nil
}

func (s *Service) lookupRequestJob(ctx context.Context, queue string, req *goipp.Message) (store.Job, error) {
	proxyJobID := requestProxyJobID(req)
	if proxyJobID == 0 {
		return store.Job{}, sql.ErrNoRows
	}
	return s.store.GetByProxyID(ctx, queue, proxyJobID)
}

func requestProxyJobID(req *goipp.Message) int {
	proxyJobID, ok := iattr.FirstInt(req.Operation, "job-id")
	if !ok {
		if jobURI, ok := iattr.FirstString(req.Operation, "job-uri"); ok {
			proxyJobID = jobIDFromURI(jobURI)
		}
	}
	return proxyJobID
}

func copiesFromAttrs(attrs goipp.Attributes) int {
	copies, ok := iattr.FirstInt(attrs, "copies")
	if !ok || copies < 1 {
		return 1
	}
	return copies
}

func nullableLogInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

func durationMillis(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func requestLogAttrs(req *goipp.Message) []any {
	attrs := []any{
		"operation_attr_count", len(req.Operation),
		"job_attr_count", len(req.Job),
	}
	if value, ok := iattr.FirstString(req.Operation, "printer-uri"); ok {
		attrs = append(attrs, "printer_uri", value)
	}
	if value, ok := iattr.FirstString(req.Operation, "job-uri"); ok {
		attrs = append(attrs, "job_uri", value)
	}
	if value, ok := iattr.FirstInt(req.Operation, "job-id"); ok {
		attrs = append(attrs, "job_id", value)
	}
	if value, ok := iattr.FirstString(req.Operation, "requesting-user-name"); ok {
		attrs = append(attrs, "user", value)
	}
	if value, ok := iattr.FirstString(req.Operation, "job-name"); ok {
		attrs = append(attrs, "job_name", value)
	}
	if value, ok := iattr.FirstString(req.Operation, "document-format"); ok {
		attrs = append(attrs, "document_format", value)
	}
	if attr, ok := iattr.Attr(req.Operation, "requested-attributes"); ok {
		attrs = append(attrs, "requested_attributes_count", len(attr.Values))
	}
	return attrs
}

func ippSuccess(resp *goipp.Message) bool {
	return goipp.Status(resp.Code) < goipp.StatusErrorBadRequest
}

func statusMessage(resp *goipp.Message) string {
	message, _ := iattr.FirstString(resp.Operation, "status-message")
	return message
}

func rewriteOperationForUpstream(attrs goipp.Attributes, upstreamURI string) goipp.Attributes {
	attrs = iattr.SetAttr(attrs, iattr.URI("printer-uri", upstreamURI))
	return attrs
}

func rewriteMappedJobOperation(attrs goipp.Attributes, job store.Job) goipp.Attributes {
	attrs = iattr.SetAttr(attrs, iattr.Integer("job-id", job.UpstreamJobID))
	if job.UpstreamJobURI != "" {
		return iattr.SetAttr(attrs, iattr.URI("job-uri", job.UpstreamJobURI))
	}
	return iattr.DropAttrs(attrs, "job-uri")
}

func rewriteResponseJob(resp *goipp.Message, queue string, proxyJobID int, printerURI string) {
	resp.Job = iattr.SetAttr(resp.Job, iattr.Integer("job-id", proxyJobID))
	resp.Job = iattr.SetAttr(resp.Job, iattr.URI("job-uri", proxyJobURI(printerURI, proxyJobID)))
	resp.Groups = nil
	_ = queue
}

func responseOperationAttrs(message string) goipp.Attributes {
	attrs := goipp.Attributes{
		goipp.MakeAttribute("attributes-charset", goipp.TagCharset, goipp.String("utf-8")),
		goipp.MakeAttribute("attributes-natural-language", goipp.TagLanguage, goipp.String("en")),
	}
	if message != "" {
		attrs = append(attrs, iattr.Text("status-message", message))
	}
	return attrs
}

func writeIPP(w http.ResponseWriter, msg *goipp.Message) {
	w.Header().Set("content-type", goipp.ContentType)
	w.WriteHeader(http.StatusOK)
	_ = msg.Encode(w)
}

func (s *Service) writeIPPError(w http.ResponseWriter, version goipp.Version, requestID uint32, status goipp.Status, message string) {
	resp := goipp.NewResponse(version, status, requestID)
	resp.Operation = responseOperationAttrs(message)
	writeIPP(w, resp)
}

func queueFromPath(raw string) string {
	clean := path.Clean(raw)
	parts := strings.Split(strings.Trim(clean, "/"), "/")
	if len(parts) >= 2 && (parts[0] == "printers" || parts[0] == "ipp") {
		return parts[1]
	}
	return ""
}

func (s *Service) proxyPrinterURI(queue string) string {
	return strings.TrimRight(s.cfg.Listen.PublicBaseURL, "/") + "/" + queue
}

func proxyJobURI(printerURI string, jobID int) string {
	return strings.TrimRight(printerURI, "/") + "/jobs/" + strconv.Itoa(jobID)
}

func jobIDFromURI(uri string) int {
	idx := strings.LastIndex(uri, "/")
	if idx < 0 || idx == len(uri)-1 {
		return 0
	}
	id, _ := strconv.Atoi(uri[idx+1:])
	return id
}
