package proxy

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/dnssd"
	iattr "github.com/grimir/golieipp/internal/ipp"
	"github.com/grimir/golieipp/internal/store"
)

type Service struct {
	cfg             *config.Config
	store           *store.Store
	upstream        *UpstreamClient
	logger          *slog.Logger
	nextID          atomic.Uint64
	startedAt       time.Time
	configChangedAt map[string]time.Time

	mu               sync.RWMutex
	capabilities     map[string]goipp.Attributes
	capabilityModels map[string]CapabilityModel
	queueHealth      map[string]queueHealth
	payloadSlots     map[string]chan struct{}
	publishers       map[string]dnssd.Publisher
	dnsRetrying      map[string]bool
	refreshing       map[string]bool
	lifecycleCtx     context.Context
	lifecycleCancel  context.CancelFunc
	loopWG           sync.WaitGroup
	dnsCtx           context.Context
	dnsCancel        context.CancelFunc
	dnsWG            sync.WaitGroup
	closing          bool
}

type queueHealth struct {
	OrdinaryReady       bool
	AirPrintReady       bool
	AirPrintReason      string
	IPPEverywhereReady  bool
	IPPEverywhereReason string
	ProfileWarnings     []string
	LastSuccess         time.Time
	LastAttempt         time.Time
	LastError           string
	DNSDegraded         bool
	DNSState            string
	DNSName             string
	DNSLastAttempt      time.Time
	DNSLastPublished    time.Time
	DNSLastError        string
	DNSAttempts         int
	IPPEligible         bool
}

type readinessResponse struct {
	Ready  bool                      `json:"ready"`
	MDNS   *mdnsReadiness            `json:"mdns,omitempty"`
	Queues map[string]queueReadiness `json:"queues"`
}

type mdnsReadiness struct {
	State string `json:"state"`
}

type queueReadiness struct {
	Optional            bool                            `json:"optional"`
	Active              bool                            `json:"active"`
	Stale               bool                            `json:"stale"`
	LastSuccess         time.Time                       `json:"last_success,omitempty"`
	LastError           string                          `json:"last_error,omitempty"`
	IPPEligible         bool                            `json:"ipp_everywhere_eligible"`
	IPPERequired        bool                            `json:"ipp_everywhere_required"`
	Profiles            profileReadinessResponseSetType `json:"profiles"`
	OrdinaryReady       bool                            `json:"ordinary_ready"`
	AirPrintReady       bool                            `json:"airprint_ready"`
	AirPrintReason      string                          `json:"airprint_reason,omitempty"`
	IPPEverywhereReady  bool                            `json:"ipp_everywhere_ready"`
	IPPEverywhereReason string                          `json:"ipp_everywhere_reason,omitempty"`
	ProfileWarnings     []string                        `json:"profile_warnings,omitempty"`
}

type profileReadinessResponse struct {
	Enabled  bool     `json:"enabled"`
	Ready    bool     `json:"ready"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

var localHostname = os.Hostname

type traceIDContextKey struct{}

const persistenceTimeout = 5 * time.Second

func NewService(cfg *config.Config, jobStore *store.Store, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	startedAt := time.Now()
	lifecycleCtx, lifecycleCancel := context.WithCancel(context.Background())
	dnsCtx, dnsCancel := context.WithCancel(context.Background())
	concurrency := cfg.Defaults.MaxConcurrentPayloadJobsPerQueue
	if concurrency <= 0 {
		concurrency = 2
	}
	service := &Service{
		cfg:              cfg,
		store:            jobStore,
		upstream:         NewUpstreamClient(logger),
		logger:           logger,
		startedAt:        startedAt,
		configChangedAt:  make(map[string]time.Time, len(cfg.Printers)),
		capabilities:     map[string]goipp.Attributes{},
		capabilityModels: map[string]CapabilityModel{},
		queueHealth:      map[string]queueHealth{},
		payloadSlots:     map[string]chan struct{}{},
		publishers:       map[string]dnssd.Publisher{},
		dnsRetrying:      map[string]bool{},
		refreshing:       map[string]bool{},
		lifecycleCtx:     lifecycleCtx,
		lifecycleCancel:  lifecycleCancel,
		dnsCtx:           dnsCtx,
		dnsCancel:        dnsCancel,
	}
	for queue := range cfg.Printers {
		service.configChangedAt[queue] = startedAt
		service.payloadSlots[queue] = make(chan struct{}, concurrency)
		service.publishers[queue] = dnssd.NewPublisher(cfg.DNSSD, logger.With("queue", queue))
	}
	if cfg.Defaults.MaxUpstreamResponseBytes > 0 {
		service.upstream.MaxResponseBytes = cfg.Defaults.MaxUpstreamResponseBytes
	}
	return service, nil
}

func (s *Service) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
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

func (s *Service) beginManagedLoop(ctx context.Context) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, nil, false
	}
	s.loopWG.Add(1)
	lifecycleCtx := s.lifecycleCtx
	s.mu.Unlock()

	if lifecycleCtx == nil {
		lifecycleCtx = context.Background()
	}
	loopCtx, cancel := context.WithCancel(lifecycleCtx)
	stopCaller := context.AfterFunc(ctx, cancel)
	finish := func() {
		stopCaller()
		cancel()
		s.loopWG.Done()
	}
	return loopCtx, finish, true
}

func (s *Service) StartRefreshLoop(ctx context.Context) {
	loopCtx, finish, started := s.beginManagedLoop(ctx)
	if !started {
		return
	}
	defer finish()
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
		s.loopWG.Add(1)
		go func() {
			defer s.loopWG.Done()
			if !s.printerAvailable(q) {
				s.refreshInBackground(loopCtx, q, p)
			}
			for {
				select {
				case <-loopCtx.Done():
					return
				case <-ticker.C:
					s.refreshInBackground(loopCtx, q, p)
				}
			}
		}()
	}
	<-loopCtx.Done()
}

func (s *Service) StartMaintenanceLoop(ctx context.Context) {
	loopCtx, finish, started := s.beginManagedLoop(ctx)
	if !started {
		return
	}
	defer finish()
	s.runMaintenance(loopCtx)
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-loopCtx.Done():
			return
		case <-ticker.C:
			s.runMaintenance(loopCtx)
		}
	}
}

func (s *Service) runMaintenance(ctx context.Context) {
	if _, err := s.store.CleanupExpired(ctx, time.Now().UTC(), s.cfg.Defaults.JobRetention); err != nil {
		s.logger.Warn("job retention cleanup failed", "error", err)
	}
	if err := s.reconcileUncertainJobs(ctx); err != nil {
		s.logger.Warn("uncertain job reconciliation failed", "error", err)
	}
}

func (s *Service) reconcileUncertainJobs(ctx context.Context) error {
	candidates, err := s.store.ReconciliationCandidates(ctx, time.Now().UTC(), 100)
	if err != nil {
		return err
	}
	byQueue := make(map[string][]store.Job)
	for _, job := range candidates {
		byQueue[job.Queue] = append(byQueue[job.Queue], job)
	}
	mapped, err := s.store.ListJobs(ctx, store.JobFilter{State: store.StateMapped})
	if err != nil {
		return err
	}
	for _, job := range mapped {
		byQueue[job.Queue] = append(byQueue[job.Queue], job)
	}
	for queue, jobs := range byQueue {
		printer, configured := s.cfg.Printers[queue]
		if !configured || !s.printerAvailable(queue) {
			continue
		}
		request := goipp.NewRequest(goipp.DefaultVersion, goipp.OpGetJobs, uint32(s.nextID.Add(1)))
		request.Operation = append(iattr.BasicOperationAttrs(printer.UpstreamURI),
			iattr.Keyword("which-jobs", "all"),
			goipp.MakeAttr("requested-attributes", goipp.TagKeyword,
				goipp.String("job-id"), goipp.String("job-uri"), goipp.String("job-name"),
				goipp.String("job-originating-user-name"), goipp.String("job-state"), goipp.String("job-state-reasons")),
		)
		response, requestErr := s.upstream.Do(ctx, printer.UpstreamURI, request, nil)
		if requestErr != nil || !ippSuccess(response) {
			continue
		}
		upstreamJobs := responseJobGroups(response)
		for _, job := range jobs {
			if job.State == store.StateUncertain {
				_ = s.store.MarkReconcileAttempt(ctx, job.ProxyJobID, time.Now().UTC())
			}
			if job.HasUpstreamJobID {
				for _, attrs := range upstreamJobs {
					upstreamID, ok := iattr.FirstInt(attrs, "job-id")
					if !ok || upstreamID != job.UpstreamJobID || !sameReconciledJob(job, attrs) {
						continue
					}
					if mapErr := s.store.MarkMapped(ctx, queue, job.ProxyJobID, job.UpstreamJobID, job.UpstreamJobURI); mapErr == nil {
						s.syncObservedJob(ctx, queue, job, attrs)
					}
					break
				}
				continue
			}
			matches := make([]goipp.Attributes, 0, 1)
			for _, attrs := range upstreamJobs {
				if sameReconciledJob(job, attrs) {
					matches = append(matches, attrs)
				}
			}
			if len(matches) != 1 {
				continue
			}
			upstreamID, ok := iattr.FirstInt(matches[0], "job-id")
			if !ok || upstreamID < 1 {
				continue
			}
			upstreamURI, _ := iattr.FirstString(matches[0], "job-uri")
			if mapErr := s.store.MarkMapped(ctx, queue, job.ProxyJobID, upstreamID, upstreamURI); mapErr != nil {
				s.logger.Warn("uncertain job reconciliation mapping failed", "queue", queue, "proxy_job_id", job.ProxyJobID, "error", mapErr)
			} else {
				job.UpstreamJobID = upstreamID
				job.HasUpstreamJobID = true
				s.syncObservedJob(ctx, queue, job, matches[0])
			}
		}
	}
	return nil
}

func sameReconciledJob(job store.Job, attrs goipp.Attributes) bool {
	// An upstream job ID is a strong identity when this helper is used for an
	// already mapped row. Unmapped uncertain rows must provide at least one
	// non-empty identity below; an empty-to-empty comparison is unsafe because
	// many printers omit optional job metadata.
	matched := job.HasUpstreamJobID && job.UpstreamJobID > 0
	if job.UpstreamJobURI != "" {
		value, ok := iattr.FirstString(attrs, "job-uri")
		if !ok || value != job.UpstreamJobURI {
			return false
		}
		matched = true
	}
	if job.JobName != "" {
		value, ok := iattr.FirstString(attrs, "job-name")
		if !ok || value != job.JobName {
			return false
		}
		matched = true
	}
	if job.RequestingUser != "" {
		value, ok := iattr.FirstString(attrs, "job-originating-user-name")
		if !ok || value != job.RequestingUser {
			return false
		}
		matched = true
	}
	return matched
}

func responseJobGroups(response *goipp.Message) []goipp.Attributes {
	if response == nil {
		return nil
	}
	if response.Groups == nil {
		if len(response.Job) == 0 {
			return nil
		}
		return []goipp.Attributes{response.Job}
	}
	var jobs []goipp.Attributes
	for _, group := range response.Groups {
		if group.Tag == goipp.TagJobGroup {
			jobs = append(jobs, group.Attrs)
		}
	}
	return jobs
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

func (s *Service) upstreamCapabilities(queue string) goipp.Attributes {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if attrs, ok := s.capabilities[queue]; ok {
		return attrs.DeepCopy()
	}
	return nil
}

func (s *Service) logPrinterAvailable(queue string, printer config.PrinterConfig) {
	s.logger.Info("printer available",
		"queue", queue,
		"display_name", printer.DisplayName,
		"printer_url", s.proxyPrinterURI(queue),
	)
}

func (s *Service) refreshOne(ctx context.Context, queue string, printer config.PrinterConfig) (retErr error) {
	start := time.Now()
	s.mu.Lock()
	s.refreshing[queue] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.refreshing, queue)
		health := s.queueHealth[queue]
		health.LastAttempt = time.Now().UTC()
		if retErr != nil {
			health.LastError = retErr.Error()
		}
		s.queueHealth[queue] = health
		s.mu.Unlock()
	}()
	s.logger.Debug("refresh upstream capabilities started",
		"queue", queue,
		"upstream_uri", redactDumpString(printer.UpstreamURI),
	)
	resp, err := s.upstream.GetPrinterAttributes(ctx, printer.UpstreamURI)
	if err != nil {
		s.logger.Debug("refresh upstream capabilities request failed",
			"queue", queue,
			"upstream_uri", redactDumpString(printer.UpstreamURI),
			"duration_ms", durationMillis(start),
			"error", err,
		)
		return err
	}
	if goipp.Status(resp.Code) >= goipp.StatusErrorBadRequest {
		s.logger.Debug("refresh upstream capabilities rejected",
			"queue", queue,
			"upstream_uri", redactDumpString(printer.UpstreamURI),
			"duration_ms", durationMillis(start),
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
		)
		return fmt.Errorf("upstream returned %s", goipp.Status(resp.Code))
	}
	effectivePolicy, policyWarnings, err := ProjectPolicyAgainstUpstream(resp.Printer, printer)
	if err != nil {
		s.logger.Warn("refresh upstream capabilities policy projection failed",
			"queue", queue,
			"upstream_uri", redactDumpString(printer.UpstreamURI),
			"duration_ms", durationMillis(start),
			"policy_unavailable", true,
			"error", err,
		)
		return err
	}
	model := NewCapabilityModel(resp.Printer, queue, s.proxyPrinterURI(queue), printer, CapabilityModelOptions{
		Operations:        proxySupportedOperations(resp.Printer),
		EffectivePolicy:   effectivePolicy,
		PracticalProfiles: true,
		Warnings:          policyWarnings,
	}, s.proxyPrinterIdentity(queue))
	profiles := model.Profiles
	if !profiles.IPPEverywhere.Ready {
		s.logger.Error("printer deemed IPP Everywhere-ineligible",
			"queue", queue,
			"upstream_uri", redactDumpString(printer.UpstreamURI),
			"reason", profiles.IPPEverywhere.Reason,
			"ipp_everywhere_mode", normalizeIPPEverywhereMode(printer.IPPEverywhereMode),
			"configuration_override", normalizeIPPEverywhereMode(printer.IPPEverywhereMode) != config.IPPEverywhereAuto,
		)
	}
	for _, warning := range model.Warnings {
		s.logger.Warn("capability profile warning",
			"queue", queue,
			"warning", warning,
		)
	}
	s.mu.Lock()
	previous, hadPrevious := s.capabilities[queue]
	previousModel, hadPreviousModel := s.capabilityModels[queue]
	if !hadPrevious || !stablePrinterAttributes(previous).Similar(stablePrinterAttributes(resp.Printer)) || !hadPreviousModel || !sameCapabilityProfiles(previousModel.Profiles, profiles) || !samePolicySnapshot(previousModel.Policy, effectivePolicy) {
		s.advanceConfigEpochLocked(queue)
	}
	s.capabilities[queue] = resp.Printer.DeepCopy()
	s.capabilityModels[queue] = model
	health := s.queueHealth[queue]
	health.LastSuccess = time.Now().UTC()
	health.LastError = ""
	health.OrdinaryReady = profiles.Ordinary.Ready
	health.AirPrintReady = profiles.AirPrint.Ready
	health.AirPrintReason = profiles.AirPrint.Reason
	health.IPPEverywhereReady = profiles.IPPEverywhere.Ready
	health.IPPEverywhereReason = profiles.IPPEverywhere.Reason
	health.ProfileWarnings = append([]string(nil), model.Warnings...)
	health.IPPEligible = profiles.IPPEverywhere.Ready
	s.queueHealth[queue] = health
	s.mu.Unlock()
	s.syncDNSPublication(ctx, queue, printer, resp.Printer, profiles)
	s.logger.Debug("refresh upstream capabilities completed",
		"queue", queue,
		"upstream_uri", redactDumpString(printer.UpstreamURI),
		"duration_ms", durationMillis(start),
		"printer_attr_count", len(resp.Printer),
	)
	return nil
}

// advanceConfigEpochLocked records a queue-local capability epoch. It must be
// called with s.mu held for writing. Keeping the timestamp monotonic protects
// printer-config-change-date-time from wall-clock adjustments and makes the
// associated integer clock stable even when refreshes happen back-to-back.
func (s *Service) advanceConfigEpochLocked(queue string) {
	if s.configChangedAt == nil {
		s.configChangedAt = make(map[string]time.Time)
	}
	now := time.Now()
	if previous := s.configChangedAt[queue]; !previous.IsZero() && !now.After(previous) {
		now = previous.Add(time.Nanosecond)
	}
	s.configChangedAt[queue] = now
}

// stablePrinterAttributes returns the portion of an upstream snapshot that
// describes printer capabilities. Operational status, clocks, counters, and
// ready inventory are deliberately excluded: they can change on every poll
// without changing the configuration visible to clients.
func stablePrinterAttributes(attrs goipp.Attributes) goipp.Attributes {
	result := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		name := strings.ToLower(strings.TrimSpace(attr.Name))
		if volatilePrinterAttribute(name) {
			continue
		}
		result = append(result, attr.DeepCopy())
	}
	return result
}

func volatilePrinterAttribute(name string) bool {
	if strings.HasSuffix(name, "-ready") {
		return true
	}
	switch name {
	case "printer-state", "printer-state-reasons", "printer-state-message", "printer-is-accepting-jobs",
		"printer-up-time", "printer-current-time", "current-time",
		"printer-config-change-time", "printer-config-change-date-time",
		"queued-job-count", "jobs-completed", "pages-completed",
		"marker-levels", "marker-high-levels", "marker-low-levels", "marker-colors", "marker-names", "marker-types",
		"input-tray", "output-bin", "printer-supply":
		return true
	default:
		return false
	}
}

func (s *Service) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) readyz(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	result := readinessResponse{Ready: true, Queues: make(map[string]queueReadiness, len(s.cfg.Printers))}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, plugin := range diagnosticPlugins() {
		if contributor, ok := plugin.(readinessContributor); ok {
			contributor.AddReadiness(&result, s)
		}
	}
	for queue, printer := range s.cfg.Printers {
		upstream, active := s.capabilities[queue]
		health := s.queueHealth[queue]
		profiles := s.profilesLocked(queue, printer, upstream)
		if !active {
			profiles = evaluateCapabilityProfiles(nil, printer, printer.Policy, nil, s.proxyPrinterURI(queue), true)
		}
		interval := printer.RefreshInterval
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		stale := active && !health.LastSuccess.IsZero() && now.Sub(health.LastSuccess) > 2*interval
		ordinaryReady := profiles.Ordinary.Ready
		result.Queues[queue] = queueReadiness{
			Optional: printer.Optional, Active: active, Stale: stale,
			LastSuccess: health.LastSuccess, LastError: safeProbeError(errors.New(health.LastError), printer.UpstreamURI),
			IPPEligible:         profiles.IPPEverywhere.Ready,
			IPPERequired:        false,
			Profiles:            profileReadinessResponseSet(profiles),
			OrdinaryReady:       ordinaryReady,
			AirPrintReady:       profiles.AirPrint.Ready,
			AirPrintReason:      profiles.AirPrint.Reason,
			IPPEverywhereReady:  profiles.IPPEverywhere.Ready,
			IPPEverywhereReason: profiles.IPPEverywhere.Reason,
			ProfileWarnings:     append([]string(nil), health.ProfileWarnings...),
		}
		if !printer.Optional && !active {
			result.Ready = false
		}
	}
	w.Header().Set("content-type", "application/json")
	w.Header().Set("cache-control", "no-cache")
	if !result.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(result)
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
	mediaType, _, mediaErr := mime.ParseMediaType(r.Header.Get("content-type"))
	if mediaErr != nil || !strings.EqualFold(mediaType, goipp.ContentType) {
		logger.Warn("ipp http request rejected", "reason", "unsupported_content_type")
		http.Error(w, "IPP requires Content-Type application/ipp", http.StatusUnsupportedMediaType)
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

	envelopeMax := s.cfg.Defaults.MaxEnvelopeBytes
	if envelopeMax <= 0 {
		envelopeMax = 1 << 20
	}
	envelopeReader := &phaseLimitReader{Reader: r.Body, Max: envelopeMax}
	req := &goipp.Message{}
	if err := req.Decode(envelopeReader); err != nil {
		logger.Warn("ipp request decode failed",
			"duration_ms", durationMillis(start),
			"error", err,
		)
		status := goipp.StatusErrorBadRequest
		if errors.Is(err, errReadLimitExceeded) {
			status = goipp.StatusErrorRequestEntity
		}
		s.writeIPPError(w, goipp.DefaultVersion, 1, status, err.Error())
		return
	}
	documentMax := s.cfg.Defaults.MaxDocumentBytes
	if documentMax <= 0 {
		documentMax = 1 << 30
	}
	if r.ContentLength >= 0 && r.ContentLength-envelopeReader.read > documentMax {
		s.writeIPPError(w, req.Version, req.RequestID, goipp.StatusErrorRequestEntity, "document exceeds configured size limit")
		return
	}
	op := goipp.Op(req.Code)
	if !supportedIPPVersion(req.Version) {
		logger.Warn("ipp request rejected", "reason", "unsupported_ipp_version", "ipp_version", req.Version.String())
		s.writeIPPError(w, closestSupportedVersion(req.Version), req.RequestID, goipp.StatusErrorVersionNotSupported, "IPP version not supported")
		return
	}
	documentReader := bufio.NewReader(&maxBytesReader{Reader: r.Body, Max: documentMax})
	hasPayload := false
	if op == goipp.OpPrintJob || op == goipp.OpSendDocument {
		if _, peekErr := documentReader.Peek(1); peekErr == nil {
			hasPayload = true
		}
	}
	if protocolErr := ValidateProtocolRequest(req, s.proxyPrinterURI(queue), hasPayload); protocolErr != nil {
		logger.Warn("ipp request rejected", "reason", "protocol_validation", "error", protocolErr)
		s.writeIPPProtocolError(w, req.Version, req.RequestID, protocolErr)
		return
	}
	if !s.printerAvailable(queue) {
		logger.Warn("ipp request rejected", "reason", "printer_not_activated")
		s.writeIPPError(w, req.Version, req.RequestID, goipp.StatusErrorPrinterIsDeactivated, "printer capabilities are not available")
		return
	}
	if !operationAvailable(proxySupportedOperations(s.upstreamCapabilities(queue)), op) {
		resp := operationNotSupported(req, "operation is not supported by this queue")
		resp.Version = req.Version
		shapeIPPResponse(resp)
		writeIPP(w, resp)
		return
	}
	if op == goipp.OpPrintJob || (op == goipp.OpSendDocument && hasPayload) {
		if protocolErr := validateExplicitDocumentFormat(req.Operation); protocolErr != nil {
			logger.Warn("ipp request rejected", "reason", "document_format_required", "error", protocolErr)
			s.writeIPPProtocolError(w, req.Version, req.RequestID, protocolErr)
			return
		}
		if protocolErr := s.validatePayloadDocumentFormat(queue, req); protocolErr != nil {
			logger.Warn("ipp request rejected", "reason", "document_format_not_admitted", "error", protocolErr)
			s.writeIPPProtocolError(w, req.Version, req.RequestID, protocolErr)
			return
		}
	}
	var staged *stagedPayload
	if r.ContentLength < 0 && (op == goipp.OpPrintJob || op == goipp.OpSendDocument) {
		var stageErr error
		staged, stageErr = stageUnknownPayload(r.Context(), documentReader, r.Body, documentMax)
		if stageErr != nil {
			status := goipp.StatusErrorServiceUnavailable
			if errors.Is(stageErr, errReadLimitExceeded) {
				status = goipp.StatusErrorRequestEntity
			}
			logger.Warn("unknown-length document staging failed", "reason", "document_staging", "error", stageErr)
			s.writeIPPError(w, req.Version, req.RequestID, status, stageErr.Error())
			return
		}
		defer func() {
			if closeErr := staged.Close(); closeErr != nil {
				logger.Warn("staged document cleanup failed", "error", closeErr)
			}
		}()
		documentReader = bufio.NewReader(staged.Reader())
		if err := r.Context().Err(); err != nil {
			logger.Warn("unknown-length document staging canceled", "reason", "request_context", "error", err)
			s.writeIPPError(w, req.Version, req.RequestID, goipp.StatusErrorServiceUnavailable, err.Error())
			return
		}
	}
	if err := r.Context().Err(); err != nil {
		logger.Warn("ipp request canceled before dispatch", "reason", "request_context", "error", err)
		s.writeIPPError(w, req.Version, req.RequestID, goipp.StatusErrorServiceUnavailable, err.Error())
		return
	}
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
		resp, err = s.withPayloadSlot(queue, req, func() (*goipp.Message, error) {
			return s.handlePrintJob(r.Context(), queue, printer, req, documentReader)
		})
	case goipp.OpCreateJob:
		resp, err = s.handleCreateJob(r.Context(), queue, printer, req)
	case goipp.OpSendDocument:
		resp, err = s.withPayloadSlot(queue, req, func() (*goipp.Message, error) {
			return s.handleSendDocument(r.Context(), queue, printer, req, documentReader, hasPayload)
		})
	case goipp.OpGetJobAttributes, goipp.OpCancelJob:
		resp, err = s.handleMappedJobOperation(r.Context(), queue, printer, req)
	case goipp.OpCloseJob:
		if !upstreamSupportsOperation(s.upstreamCapabilities(queue), op) {
			resp = operationNotSupported(req, "Close-Job is not supported by the upstream printer")
		} else {
			resp, err = s.handleMappedJobOperation(r.Context(), queue, printer, req)
		}
	case goipp.OpGetJobs:
		resp, err = s.handleGetJobs(r.Context(), queue, printer, req)
	case goipp.OpCancelMyJobs:
		resp, err = s.handleCancelMyJobs(r.Context(), queue, printer, req)
	case goipp.OpIdentifyPrinter:
		if !upstreamSupportsOperation(s.upstreamCapabilities(queue), op) {
			resp = operationNotSupported(req, "Identify-Printer is not supported by the upstream printer")
		} else {
			resp, err = s.forwardPrinterOperation(r.Context(), printer, req)
		}
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
	// The proxy is the client-facing Printer object. An upstream may legally
	// negotiate down to IPP/1.1, but that version belongs on the upstream hop;
	// answer the client using the version it successfully requested here.
	resp.Version = req.Version
	shapeIPPResponse(resp)
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

func supportedIPPVersion(version goipp.Version) bool {
	switch version {
	case goipp.MakeVersion(1, 0), goipp.MakeVersion(1, 1), goipp.MakeVersion(2, 0):
		return true
	default:
		return false
	}
}

func validateRequiredOperationAttrs(attrs goipp.Attributes, op goipp.Op) error {
	if len(attrs) < 2 || !singleValueAttr(attrs[0], "attributes-charset", goipp.TagCharset) ||
		!singleValueAttr(attrs[1], "attributes-natural-language", goipp.TagLanguage) {
		return errors.New("attributes-charset and attributes-natural-language must be the first two operation attributes")
	}
	if printerOperationRequiresURI(op) {
		if len(attrs) < 3 || !singleValueAttr(attrs[2], "printer-uri", goipp.TagURI) {
			return errors.New("printer-uri operation attribute is required")
		}
	}
	return nil
}

func singleValueAttr(attr goipp.Attribute, name string, tag goipp.Tag) bool {
	return strings.EqualFold(attr.Name, name) && len(attr.Values) == 1 && attr.Values[0].T == tag
}

func printerOperationRequiresURI(op goipp.Op) bool {
	switch op {
	case goipp.OpPrintJob,
		goipp.OpValidateJob,
		goipp.OpCreateJob,
		goipp.OpGetJobs,
		goipp.OpGetPrinterAttributes,
		goipp.OpCancelMyJobs,
		goipp.OpIdentifyPrinter:
		return true
	default:
		return false
	}
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
	filtered := s.clientCapabilities(queue, printer, upstream)
	s.mu.RLock()
	dnsName := s.queueHealth[queue].DNSName
	s.mu.RUnlock()
	if dnsName != "" {
		filtered = iattr.SetAttr(filtered, iattr.Name("printer-dns-sd-name", dnsName))
	}
	resp.Printer = FilterRequestedPrinterAttributes(filtered, req.Operation)
	return resp, nil
}

func upstreamClaimsIPPEverywhere(attrs goipp.Attributes) bool {
	eligible, _ := ippEverywhereClaimEligibility(attrs)
	return eligible
}

func ippEverywhereClaimEligibility(attrs goipp.Attributes) (bool, string) {
	var (
		claimFound bool
		claims     []goipp.Attribute
	)
	for _, attr := range attrs {
		if !strings.EqualFold(attr.Name, "ipp-features-supported") {
			continue
		}
		claims = append(claims, attr)
	}
	if len(claims) == 0 {
		return false, "upstream did not provide ipp-features-supported"
	}
	if len(claims) > 1 {
		return false, "upstream provided duplicate ipp-features-supported attributes"
	}
	if len(claims[0].Values) == 0 {
		return false, "upstream provided an empty ipp-features-supported attribute"
	}
	for index, value := range claims[0].Values {
		if value.T != goipp.TagKeyword {
			return false, fmt.Sprintf("upstream ipp-features-supported value %d has tag %s, want keyword", index, value.T)
		}
		feature, ok := value.V.(goipp.String)
		if !ok || strings.TrimSpace(string(feature)) == "" {
			return false, fmt.Sprintf("upstream ipp-features-supported value %d is not a non-empty keyword", index)
		}
		if string(feature) == "ipp-everywhere" {
			claimFound = true
		}
	}
	if !claimFound {
		return false, "upstream ipp-features-supported does not include ipp-everywhere"
	}
	return true, ""
}

func (s *Service) syncDNSPublication(ctx context.Context, queue string, printer config.PrinterConfig, upstream goipp.Attributes, profiles CapabilityProfiles) {
	publisher := s.publishers[queue]
	if publisher == nil {
		return
	}
	// The ordinary IPP service is the base publication. AirPrint and IPP
	// Everywhere only control their own subtypes and never suppress this base
	// endpoint when their profile checks fail.
	if !profiles.Ordinary.Ready || !printer.DNSSD || s.cfg.DNSSD.Mode == config.DNSModeOff {
		status, withdrawErr := publisher.Withdraw(ctx)
		if withdrawErr != nil && status.Err == nil {
			status.Err = withdrawErr
			status.State = dnssd.StateDegraded
		}
		s.recordDNSStatus(queue, status)
		if status.State == dnssd.StateDegraded {
			s.scheduleDNSWithdrawRetry(queue, publisher)
		}
		return
	}
	input, err := s.dnsServiceInputWithProfiles(queue, printer, upstream, profiles)
	if err != nil {
		s.recordDNSStatus(queue, dnssd.Status{State: dnssd.StateDegraded, Err: err})
		return
	}
	status, publishErr := publisher.Update(ctx, input)
	if publishErr != nil && status.Err == nil {
		status.Err = publishErr
		status.State = dnssd.StateDegraded
	}
	s.recordDNSStatus(queue, status)
	if status.State == dnssd.StateDegraded {
		s.scheduleDNSRetry(ctx, queue, publisher, input)
	}
}

func (s *Service) dnsServiceInput(queue string, printer config.PrinterConfig, upstream goipp.Attributes) (dnssd.ServiceInput, error) {
	profiles := s.profilesFor(queue, printer, upstream)
	return s.dnsServiceInputWithProfiles(queue, printer, upstream, profiles)
}

func (s *Service) dnsServiceInputWithProfiles(queue string, printer config.PrinterConfig, upstream goipp.Attributes, profiles CapabilityProfiles) (dnssd.ServiceInput, error) {
	printerURI := s.proxyPrinterURI(queue)
	parsed, err := url.Parse(printerURI)
	if err != nil || parsed.Hostname() == "" {
		return dnssd.ServiceInput{}, fmt.Errorf("invalid public printer URI %q", printerURI)
	}
	port := 631
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil || port < 1 || port > 65535 {
			return dnssd.ServiceInput{}, fmt.Errorf("invalid public printer port")
		}
	}
	clientAttrs := s.clientCapabilities(queue, printer, upstream)
	hostname := s.cfg.DNSSD.Hostname
	if hostname == "" {
		resolved, hostnameErr := localHostname()
		if hostnameErr != nil || strings.TrimSpace(resolved) == "" {
			s.logger.Warn("local hostname lookup failed for DNS-SD; using public endpoint host",
				"queue", queue,
				"error", hostnameErr,
				"fallback_hostname", parsed.Hostname(),
			)
			hostname = parsed.Hostname()
		} else {
			hostname = strings.TrimSuffix(strings.TrimSpace(resolved), ".")
		}
	}
	publicHost := parsed.Hostname()
	if !hostsMatchOrAreAllowedAliases(hostname, publicHost, s.cfg.DNSSD.AllowedAliases) {
		s.logger.Warn("DNS-SD hostname differs from public endpoint host",
			"queue", queue,
			"dns_sd_hostname", hostname,
			"public_endpoint_host", publicHost,
		)
	}
	geoLocation, _ := iattr.FirstString(upstream, "printer-geo-location")
	if geoLocation == "" {
		geoLocation = printer.GeoLocation
	}
	return dnssd.ServiceInput{
		Name: printer.DisplayName, Hostname: hostname,
		Port: uint16(port), IPPS: false, Interface: s.cfg.DNSSD.Interface,
		TXT:         printerDNSSDTXTForProfiles(parsed.EscapedPath(), printer.DisplayName, printer.Location, clientAttrs, profiles),
		GeoLocation: geoLocation,
		Profiles:    dnssd.PublicationProfiles{Ordinary: profiles.Ordinary.Ready, AirPrint: profiles.AirPrint.Ready, IPPEverywhere: profiles.IPPEverywhere.Ready},
		ProfilesSet: true,
	}, nil
}

func hostsMatchOrAreAllowedAliases(dnsSDHostname, publicEndpointHost string, allowedAliases []string) bool {
	dnsHost := canonicalDNSHost(dnsSDHostname)
	publicHost := canonicalDNSHost(publicEndpointHost)
	if dnsHost == publicHost {
		return true
	}
	for _, alias := range allowedAliases {
		canonicalAlias := canonicalDNSHost(alias)
		if canonicalAlias == dnsHost || canonicalAlias == publicHost {
			return true
		}
	}
	return false
}

func canonicalDNSHost(raw string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(raw), "."))
}

func (s *Service) clientCapabilities(queue string, printer config.PrinterConfig, upstream goipp.Attributes) goipp.Attributes {
	s.mu.RLock()
	model, ok := s.capabilityModels[queue]
	s.mu.RUnlock()
	var attrs goipp.Attributes
	if ok && len(model.Attributes) > 0 {
		// The capability snapshot owns the stable printer description, while
		// proxy-owned identity clocks are intentionally live per response.
		// Refresh the identity after a capability epoch advances so a committed
		// model cannot pin clients to the pre-refresh timestamp.
		model.Identity = s.proxyPrinterIdentity(queue)
		model.Attributes = nil
		attrs = FilterPrinterAttributesWithModel(model)
	} else {
		attrs = FilterPrinterAttributesWithOperations(upstream.DeepCopy(), queue, s.proxyPrinterURI(queue), printer, proxySupportedOperations(upstream), s.proxyPrinterIdentity(queue))
	}
	// The current request path does not preserve an overrides collection as a
	// supported job-template attribute. Do not claim support in the client view
	// until a fully validated forwarding path is enabled.
	return iattr.DropAttrs(attrs, "overrides-supported")
}

func (s *Service) profilesFor(queue string, printer config.PrinterConfig, upstream goipp.Attributes) CapabilityProfiles {
	s.mu.RLock()
	profiles := s.profilesLocked(queue, printer, upstream)
	s.mu.RUnlock()
	return profiles
}

// profilesLocked returns the last committed profile snapshot. When a caller
// constructs a Service test double by populating capabilities directly, derive
// a deterministic fallback without mutating service state.
func (s *Service) profilesLocked(queue string, printer config.PrinterConfig, upstream goipp.Attributes) CapabilityProfiles {
	if model, ok := s.capabilityModels[queue]; ok {
		return model.Profiles
	}
	return evaluateCapabilityProfiles(upstream, printer, printer.Policy, proxySupportedOperations(upstream), s.proxyPrinterURI(queue), false)
}

func proxySupportedOperations(upstream goipp.Attributes) []goipp.Op {
	// Get-Printer-Attributes is served from the proxy's validated capability
	// snapshot and does not depend on an upstream operations-supported entry
	// once the initial capability probe has succeeded.
	operations := []goipp.Op{goipp.OpGetPrinterAttributes}
	for _, operation := range []goipp.Op{
		goipp.OpPrintJob, goipp.OpValidateJob, goipp.OpCancelJob, goipp.OpGetJobAttributes, goipp.OpGetJobs,
	} {
		if upstreamSupportsOperation(upstream, operation) {
			operations = append(operations, operation)
		}
	}
	if upstreamSupportsOperation(upstream, goipp.OpCreateJob) && upstreamSupportsOperation(upstream, goipp.OpSendDocument) {
		operations = append(operations, goipp.OpCreateJob, goipp.OpSendDocument)
	}
	if upstreamSupportsOperation(upstream, goipp.OpCancelJob) {
		operations = append(operations, goipp.OpCancelMyJobs)
	}
	for _, operation := range []goipp.Op{goipp.OpCloseJob, goipp.OpIdentifyPrinter} {
		if upstreamSupportsOperation(upstream, operation) {
			operations = append(operations, operation)
		}
	}
	return operations
}

func operationAvailable(operations []goipp.Op, wanted goipp.Op) bool {
	for _, operation := range operations {
		if operation == wanted {
			return true
		}
	}
	return false
}

// validatePayloadDocumentFormat admits only formats from the committed,
// proxy-filtered capability snapshot. A Service created in package tests can
// still populate capabilities directly; in that compatibility mode there is
// no committed effective-format set to enforce.
func (s *Service) validatePayloadDocumentFormat(queue string, req *goipp.Message) *ProtocolError {
	format, ok := iattr.FirstString(req.Operation, "document-format")
	if !ok || strings.TrimSpace(format) == "" {
		return &ProtocolError{
			Status:      goipp.StatusErrorDocumentFormatNotSupported,
			Message:     "document-format is not admitted for a payload-bearing operation",
			Unsupported: attrsNamed(req.Operation, "document-format"),
		}
	}
	formatKey := strings.ToLower(strings.TrimSpace(format))

	s.mu.RLock()
	model, hasModel := s.capabilityModels[queue]
	health := s.queueHealth[queue]
	printer := s.cfg.Printers[queue]
	s.mu.RUnlock()
	if !hasModel {
		return nil
	}

	formats, present := effectiveFormatSet(model.Upstream, model.Policy)
	if !present || len(formats) == 0 {
		return &ProtocolError{
			Status:  goipp.StatusErrorDocumentFormatNotSupported,
			Message: "the proxy has no admitted document formats",
		}
	}
	if _, supported := formats[formatKey]; !supported {
		return &ProtocolError{
			Status:      goipp.StatusErrorDocumentFormatNotSupported,
			Message:     fmt.Sprintf("document-format %q is not supported by the proxy", format),
			Unsupported: attrsNamed(req.Operation, "document-format"),
		}
	}

	interval := printer.RefreshInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if !health.LastSuccess.IsZero() && time.Since(health.LastSuccess) > 2*interval {
		s.logger.Warn("forwarding document format from stale capability snapshot",
			"queue", queue,
			"document_format", format,
			"stale", true,
			"last_success", health.LastSuccess,
			"last_error", health.LastError,
		)
	}
	return nil
}

func (s *Service) recordDNSStatus(queue string, status dnssd.Status) {
	s.mu.Lock()
	defer s.mu.Unlock()
	health := s.queueHealth[queue]
	health.DNSLastAttempt = time.Now().UTC()
	health.DNSAttempts = status.Attempts
	health.DNSState = string(status.State)
	health.DNSDegraded = status.State == dnssd.StateDegraded
	if status.Err != nil {
		health.DNSLastError = status.Err.Error()
	}
	if status.State == dnssd.StatePublished {
		health.DNSLastPublished = health.DNSLastAttempt
		health.DNSLastError = ""
	} else if status.State == dnssd.StateWithdrawn || status.State == dnssd.StateDisabled {
		health.DNSLastError = ""
	}
	if status.State == dnssd.StateWithdrawn || status.State == dnssd.StateDisabled {
		health.DNSName = ""
	} else if status.Name != "" {
		health.DNSName = status.Name
	}
	s.queueHealth[queue] = health
}

func (s *Service) scheduleDNSRetry(ctx context.Context, queue string, publisher dnssd.Publisher, input dnssd.ServiceInput) {
	s.mu.Lock()
	if s.closing || s.dnsRetrying[queue] {
		s.mu.Unlock()
		return
	}
	s.dnsRetrying[queue] = true
	s.dnsWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.dnsWG.Done()
		defer func() {
			s.mu.Lock()
			s.dnsRetrying[queue] = false
			s.mu.Unlock()
		}()
		delay := time.Second
		for attempt := 0; attempt < 6; attempt++ {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-s.dnsCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			s.mu.RLock()
			printer := s.cfg.Printers[queue]
			profiles := s.profilesLocked(queue, printer, s.capabilities[queue])
			s.mu.RUnlock()
			if !profiles.Ordinary.Ready || !printer.DNSSD || s.cfg.DNSSD.Mode == config.DNSModeOff {
				return
			}
			latestUpstream := s.upstreamCapabilities(queue)
			latestInput, inputErr := s.dnsServiceInputWithProfiles(queue, printer, latestUpstream, profiles)
			if inputErr != nil {
				s.recordDNSStatus(queue, dnssd.Status{State: dnssd.StateDegraded, Name: input.Name, Err: inputErr})
				continue
			}
			status, err := publisher.Update(s.dnsCtx, latestInput)
			if err != nil && status.Err == nil {
				status.Err = err
			}
			s.recordDNSStatus(queue, status)
			if status.State != dnssd.StateDegraded {
				return
			}
			if delay < 16*time.Second {
				delay *= 2
			}
		}
	}()
}

func (s *Service) scheduleDNSWithdrawRetry(queue string, publisher dnssd.Publisher) {
	s.mu.Lock()
	if s.closing || s.dnsRetrying[queue] {
		s.mu.Unlock()
		return
	}
	s.dnsRetrying[queue] = true
	s.dnsWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.dnsWG.Done()
		defer func() {
			s.mu.Lock()
			s.dnsRetrying[queue] = false
			s.mu.Unlock()
		}()
		delay := time.Second
		for attempt := 0; attempt < 6; attempt++ {
			timer := time.NewTimer(delay)
			select {
			case <-s.dnsCtx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			status, err := publisher.Withdraw(s.dnsCtx)
			if err != nil && status.Err == nil {
				status.Err = err
				status.State = dnssd.StateDegraded
			}
			s.recordDNSStatus(queue, status)
			if status.State != dnssd.StateDegraded {
				return
			}
			if delay < 16*time.Second {
				delay *= 2
			}
		}
	}()
}

func (s *Service) Close() error {
	s.mu.Lock()
	s.closing = true
	if s.lifecycleCancel != nil {
		s.lifecycleCancel()
	}
	if s.dnsCancel != nil {
		s.dnsCancel()
	}
	s.mu.Unlock()
	s.loopWG.Wait()
	s.dnsWG.Wait()
	var joined error
	for queue, publisher := range s.publishers {
		if publisher == nil {
			continue
		}
		if err := publisher.Close(); err != nil {
			joined = errors.Join(joined, fmt.Errorf("withdraw DNS-SD queue %s: %w", queue, err))
		}
	}
	return joined
}

func (s *Service) handleValidateJob(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	normalized, rejected := s.normalizeJobRequest(queue, printer, req)
	if rejected != nil {
		return rejected, nil
	}
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized.Attrs
	req.Groups = nil
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	applyNormalizationResult(resp, normalized)
	return resp, err
}

func (s *Service) handlePrintJob(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message, payload io.Reader) (*goipp.Message, error) {
	if protocolErr := validateExplicitDocumentFormat(req.Operation); protocolErr != nil {
		return protocolResponse(req, protocolErr.Status, protocolErr.Message), nil
	}
	if protocolErr := s.validatePayloadDocumentFormat(queue, req); protocolErr != nil {
		return protocolResponse(req, protocolErr.Status, protocolErr.Message), nil
	}
	normalizedResult, rejected := s.normalizeJobRequest(queue, printer, req)
	if rejected != nil {
		return rejected, nil
	}
	normalized, normLog := normalizedResult.Attrs, normalizedResult.Log
	user, _ := iattr.FirstString(req.Operation, "requesting-user-name")
	jobName, _ := iattr.FirstString(req.Operation, "job-name")
	format, _ := iattr.FirstString(req.Operation, "document-format")
	copies := copiesFromAttrs(normalized)
	proxyID, err := s.store.Reserve(ctx, store.Job{
		Queue: queue, QueueOwner: queue, RequestingUser: user, JobName: jobName,
		DocumentFormat: format, Copies: copies,
	})
	if err != nil {
		return nil, fmt.Errorf("reserve proxy job: %w", err)
	}
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized
	req.Groups = nil

	recorder, err := newPayloadRecorder(payload)
	if err != nil {
		_ = s.store.MarkTerminal(ctx, queue, proxyID, store.StateFailed, "", err.Error())
		return nil, err
	}
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, recorder.Reader())
	persistCtx, cancelPersist := s.persistenceContext()
	defer cancelPersist()
	meta, metaErr := recorder.Finish(format, copies)
	if metaErr != nil {
		s.logger.Warn("payload metadata extraction failed", "queue", queue, "job_name", jobName, "error", metaErr)
	}
	if err != nil {
		_ = s.store.MarkUncertain(persistCtx, queue, proxyID, err.Error())
		return nil, err
	}
	if !ippSuccess(resp) {
		_ = s.store.MarkTerminal(persistCtx, queue, proxyID, store.StateFailed, goipp.Status(resp.Code).String(), statusMessage(resp))
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
	applyNormalizationResult(resp, normalizedResult)
	upstreamJobID, _ := iattr.FirstInt(resp.Job, "job-id")
	upstreamJobURI, _ := iattr.FirstString(resp.Job, "job-uri")
	if upstreamJobID > 0 {
		if mapErr := s.store.MarkMapped(persistCtx, queue, proxyID, upstreamJobID, upstreamJobURI); mapErr != nil {
			s.logger.Error("critical: upstream accepted job but mapping persistence failed", "queue", queue, "proxy_job_id", proxyID, "error", mapErr)
			_ = s.store.MarkUncertain(persistCtx, queue, proxyID, mapErr.Error())
		}
	} else {
		s.logger.Error("critical: upstream accepted job without a job-id", "queue", queue, "proxy_job_id", proxyID)
		_ = s.store.MarkUncertain(persistCtx, queue, proxyID, "upstream accepted job without job-id")
	}
	if metaErr := s.store.UpdatePayloadMetadataWithLastDocument(persistCtx, queue, proxyID, format, meta.Bytes, meta.PageCount, meta.Copies, meta.EstimatedImpressions, true); metaErr != nil {
		s.logger.Error("critical: upstream accepted job but payload metadata persistence failed", "queue", queue, "proxy_job_id", proxyID, "error", metaErr)
		_ = s.store.MarkUncertain(persistCtx, queue, proxyID, metaErr.Error())
	}
	rewriteResponseJob(resp, queue, proxyID, s.proxyPrinterURI(queue))
	attrs := normLog.Attrs()
	args := []any{
		"queue", queue, "user", user, "job_name", jobName, "document_format", format,
		"upstream_job_id", upstreamJobID, "proxy_job_id", proxyID, "payload_bytes", meta.Bytes,
		"page_count", nullableLogInt(meta.PageCount), "copies", meta.Copies,
		"estimated_impressions", nullableLogInt(meta.EstimatedImpressions),
	}
	for _, attr := range attrs {
		args = append(args, attr.Key, attr.Value.Any())
	}
	s.logger.Info("job submitted", args...)
	return resp, nil
}

func (s *Service) handleCreateJob(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	normalizedResult, rejected := s.normalizeJobRequest(queue, printer, req)
	if rejected != nil {
		return rejected, nil
	}
	normalized, normLog := normalizedResult.Attrs, normalizedResult.Log
	user, _ := iattr.FirstString(req.Operation, "requesting-user-name")
	jobName, _ := iattr.FirstString(req.Operation, "job-name")
	format, _ := iattr.FirstString(req.Operation, "document-format")
	copies := copiesFromAttrs(normalized)
	proxyID, err := s.store.Reserve(ctx, store.Job{
		Queue: queue, QueueOwner: queue, RequestingUser: user, JobName: jobName,
		DocumentFormat: format, Copies: copies,
	})
	if err != nil {
		return nil, fmt.Errorf("reserve proxy job: %w", err)
	}
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Job = normalized
	req.Groups = nil

	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	persistCtx, cancelPersist := s.persistenceContext()
	defer cancelPersist()
	if err != nil {
		_ = s.store.MarkUncertain(persistCtx, queue, proxyID, err.Error())
		return nil, err
	}
	if !ippSuccess(resp) {
		_ = s.store.MarkTerminal(persistCtx, queue, proxyID, store.StateFailed, goipp.Status(resp.Code).String(), statusMessage(resp))
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
	applyNormalizationResult(resp, normalizedResult)
	upstreamJobID, _ := iattr.FirstInt(resp.Job, "job-id")
	upstreamJobURI, _ := iattr.FirstString(resp.Job, "job-uri")
	if upstreamJobID > 0 {
		if mapErr := s.store.MarkMapped(persistCtx, queue, proxyID, upstreamJobID, upstreamJobURI); mapErr != nil {
			s.logger.Error("critical: upstream created job but mapping persistence failed", "queue", queue, "proxy_job_id", proxyID, "error", mapErr)
			_ = s.store.MarkUncertain(persistCtx, queue, proxyID, mapErr.Error())
		}
	} else {
		s.logger.Error("critical: upstream created job without a job-id", "queue", queue, "proxy_job_id", proxyID)
		_ = s.store.MarkUncertain(persistCtx, queue, proxyID, "upstream created job without job-id")
	}
	rewriteResponseJob(resp, queue, proxyID, s.proxyPrinterURI(queue))
	attrs := normLog.Attrs()
	args := []any{"queue", queue, "user", user, "job_name", jobName, "document_format", format, "upstream_job_id", upstreamJobID, "proxy_job_id", proxyID, "copies", copies}
	for _, attr := range attrs {
		args = append(args, attr.Key, attr.Value.Any())
	}
	s.logger.Info("job created", args...)
	return resp, nil
}

func (s *Service) normalizeJobRequest(queue string, printer config.PrinterConfig, req *goipp.Message) (NormalizationResult, *goipp.Message) {
	fidelity := false
	if attr, ok := iattr.Attr(req.Operation, "ipp-attribute-fidelity"); ok && len(attr.Values) == 1 {
		if value, valueOK := attr.Values[0].V.(goipp.Boolean); valueOK {
			fidelity = bool(value)
		}
	}
	policy := printer.Policy
	s.mu.RLock()
	if model, ok := s.capabilityModels[queue]; ok {
		policy = model.Policy
	}
	s.mu.RUnlock()
	result, err := NormalizeJobAttrsWithOptions(req.Job, NormalizationOptions{
		Policy: policy, Upstream: s.upstreamCapabilities(queue),
		DropVendorAttrs:  printer.Passthrough.DropVendorAttrs,
		PreserveJobAttrs: printer.Passthrough.PreserveJobAttrs,
		Fidelity:         fidelity, FidelityMode: FidelityMode(policy.FidelityMode),
	})
	if err == nil {
		return result, nil
	}
	status := goipp.StatusErrorAttributesOrValues
	unsupported := goipp.Attributes(nil)
	if normalizationErr, ok := err.(*NormalizationError); ok {
		status = normalizationErr.Status
		unsupported = normalizationErr.Unsupported
	}
	resp := protocolResponse(req, status, err.Error())
	resp.Unsupported = unsupported
	return NormalizationResult{}, resp
}

func applyNormalizationResult(resp *goipp.Message, result NormalizationResult) {
	if resp == nil || !ippSuccess(resp) || !result.Substituted {
		return
	}
	if goipp.Status(resp.Code) == goipp.StatusOk {
		resp.Code = goipp.Code(goipp.StatusOkIgnoredOrSubstituted)
	}
	resp.Unsupported = append(resp.Unsupported, result.Unsupported...)
	if resp.Groups != nil && len(result.Unsupported) > 0 {
		resp.Groups = append(resp.Groups, goipp.Group{Tag: goipp.TagUnsupportedGroup, Attrs: result.Unsupported.DeepCopy()})
	}
}

func (s *Service) handleSendDocument(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message, payload io.Reader, hasPayload bool) (*goipp.Message, error) {
	if hasPayload {
		if protocolErr := validateExplicitDocumentFormat(req.Operation); protocolErr != nil {
			return protocolResponse(req, protocolErr.Status, protocolErr.Message), nil
		}
		if protocolErr := s.validatePayloadDocumentFormat(queue, req); protocolErr != nil {
			return protocolResponse(req, protocolErr.Status, protocolErr.Message), nil
		}
	}
	proxyJobID := requestProxyJobID(req)
	job, err := s.lookupRequestJob(ctx, queue, req)
	if err != nil {
		return nil, err
	}
	if proxyJobID == 0 {
		proxyJobID = job.ProxyJobID
	}
	if !job.HasUpstreamJobID || job.UpstreamJobID < 1 || job.State == store.StateReserved || job.State == store.StateUncertain || store.IsTerminalState(job.State) {
		return protocolResponse(req, goipp.StatusErrorNotPossible, "job is not available for document submission"), nil
	}
	lastDocument := false
	if attr, ok := iattr.Attr(req.Operation, "last-document"); ok && len(attr.Values) == 1 {
		if value, valueOK := attr.Values[0].V.(goipp.Boolean); valueOK {
			lastDocument = bool(value)
		}
	}
	if job.LastDocument || (job.DocumentCount > 0 && hasPayload) {
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorMultipleJobsNotSupported, req.RequestID)
		resp.Operation = responseOperationAttrs("this proxy accepts exactly one document per job")
		return resp, nil
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
		_ = s.store.MarkUncertain(ctx, queue, proxyJobID, err.Error())
		return nil, err
	}
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, recorder.Reader())
	persistCtx, cancelPersist := s.persistenceContext()
	defer cancelPersist()
	meta, metaErr := recorder.Finish(format, job.Copies)
	if metaErr != nil {
		s.logger.Warn("payload metadata extraction failed", "queue", queue, "proxy_job_id", proxyJobID, "error", metaErr)
	}
	if err != nil {
		_ = s.store.MarkUncertain(persistCtx, queue, proxyJobID, err.Error())
		return nil, err
	}
	if !ippSuccess(resp) {
		if proxyJobID > 0 {
			if stateErr := s.store.UpdateState(persistCtx, queue, proxyJobID, "failed"); stateErr != nil {
				s.logger.Warn("record rejected document state failed",
					"queue", queue,
					"proxy_job_id", proxyJobID,
					"error", stateErr,
				)
			}
		}
		s.logger.Warn("send document rejected by upstream",
			"queue", queue,
			"proxy_job_id", proxyJobID,
			"upstream_job_id", job.UpstreamJobID,
			"document_format", format,
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
			"payload_bytes", meta.Bytes,
		)
		rewriteResponseJob(resp, queue, proxyJobID, s.proxyPrinterURI(queue))
		return resp, nil
	}
	if proxyJobID > 0 {
		if err := s.store.UpdatePayloadMetadataWithLastDocument(persistCtx, queue, proxyJobID, format, meta.Bytes, meta.PageCount, meta.Copies, meta.EstimatedImpressions, lastDocument); err != nil {
			s.logger.Error("critical: upstream accepted document but metadata persistence failed", "queue", queue, "proxy_job_id", proxyJobID, "error", err)
			_ = s.store.MarkUncertain(persistCtx, queue, proxyJobID, err.Error())
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
	rewriteResponseJob(resp, queue, proxyJobID, s.proxyPrinterURI(queue))
	return resp, nil
}

func (s *Service) handleGetJobs(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	requestUser, _ := iattr.FirstString(req.Operation, "requesting-user-name")
	limit, _ := iattr.FirstInt(req.Operation, "limit")
	requested := requestedJobAttributes(req.Operation)
	myJobs := false
	if attr, ok := iattr.Attr(req.Operation, "my-jobs"); ok && len(attr.Values) == 1 {
		if value, valueOK := attr.Values[0].V.(goipp.Boolean); valueOK {
			myJobs = bool(value)
		}
	}
	// Upstream limiting/projection must not happen before proxy-owned jobs are
	// filtered, otherwise hidden jobs consume the client's limit or identity
	// fields needed for mapping disappear.
	req.Operation = iattr.DropAttrs(req.Operation, "limit", "requested-attributes")
	req.Operation = append(req.Operation, goipp.MakeAttribute("requested-attributes", goipp.TagKeyword, goipp.String("all")))
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Groups = nil
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	if err != nil || !ippSuccess(resp) {
		return resp, err
	}
	persistCtx, cancelPersist := s.persistenceContext()
	defer cancelPersist()
	printerURI := s.proxyPrinterURI(queue)
	kept := 0
	filterJob := func(attrs goipp.Attributes) (goipp.Attributes, bool) {
		if limit > 0 && kept >= limit {
			return nil, false
		}
		upstreamID, ok := iattr.FirstInt(attrs, "job-id")
		if !ok {
			return nil, false
		}
		job, lookupErr := s.store.GetByUpstreamID(persistCtx, queue, upstreamID)
		if lookupErr != nil || (myJobs && job.RequestingUser != requestUser) {
			return nil, false
		}
		s.syncObservedJob(persistCtx, queue, job, attrs)
		kept++
		return projectJobAttributes(rewriteJobAttrs(attrs, job.ProxyJobID, printerURI), requested), true
	}
	if resp.Groups != nil {
		groups := make(goipp.Groups, 0, len(resp.Groups))
		resp.Job = nil
		for _, group := range resp.Groups {
			if group.Tag != goipp.TagJobGroup {
				groups = append(groups, group)
				continue
			}
			attrs, keep := filterJob(group.Attrs)
			if keep {
				group.Attrs = attrs
				groups = append(groups, group)
				resp.Job = append(resp.Job, attrs...)
			}
		}
		resp.Groups = groups
	} else if attrs, keep := filterJob(resp.Job); keep {
		resp.Job = attrs
	} else {
		resp.Job = nil
	}
	return resp, nil
}

func (s *Service) handleCancelMyJobs(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	user, ok := iattr.FirstString(req.Operation, "requesting-user-name")
	if !ok || user == "" {
		return protocolResponse(req, goipp.StatusErrorBadRequest, "requesting-user-name is required"), nil
	}
	jobs, err := s.store.ListJobs(ctx, store.JobFilter{Queue: queue, RequestingUser: user, ExcludeTerminal: true})
	if err != nil {
		return nil, err
	}
	failed := 0
	for _, job := range jobs {
		if !job.HasUpstreamJobID || job.UpstreamJobID < 1 {
			continue
		}
		cancel := goipp.NewRequest(req.Version, goipp.OpCancelJob, uint32(s.nextID.Add(1)))
		cancel.Operation = append(iattr.BasicOperationAttrs(printer.UpstreamURI), iattr.Name("requesting-user-name", user), iattr.Integer("job-id", job.UpstreamJobID))
		cancelResp, cancelErr := s.upstream.Do(ctx, printer.UpstreamURI, cancel, nil)
		persistCtx, cancelPersist := s.persistenceContext()
		if cancelErr != nil {
			_ = s.store.MarkUncertain(persistCtx, queue, job.ProxyJobID, cancelErr.Error())
			cancelPersist()
			failed++
			continue
		}
		if !ippSuccess(cancelResp) {
			cancelPersist()
			failed++
			continue
		}
		if stateErr := s.store.MarkTerminal(persistCtx, queue, job.ProxyJobID, store.StateCanceled, ""); stateErr != nil {
			s.logger.Warn("record canceled job state failed", "queue", queue, "proxy_job_id", job.ProxyJobID, "error", stateErr)
		}
		cancelPersist()
	}
	if failed > 0 {
		return protocolResponse(req, goipp.StatusErrorNotPossible, fmt.Sprintf("%d jobs could not be canceled", failed)), nil
	}
	return protocolResponse(req, goipp.StatusOk, ""), nil
}

func protocolResponse(req *goipp.Message, status goipp.Status, message string) *goipp.Message {
	resp := goipp.NewResponse(req.Version, status, req.RequestID)
	resp.Operation = responseOperationAttrs(message)
	return resp
}

func operationNotSupported(req *goipp.Message, message string) *goipp.Message {
	return protocolResponse(req, goipp.StatusErrorOperationNotSupported, message)
}

func upstreamSupportsOperation(attrs goipp.Attributes, operation goipp.Op) bool {
	var (
		operationsAttr goipp.Attribute
		attrCount      int
	)
	for _, attr := range attrs {
		if !strings.EqualFold(attr.Name, "operations-supported") {
			continue
		}
		attrCount++
		operationsAttr = attr
	}
	if attrCount != 1 || len(operationsAttr.Values) == 0 {
		return false
	}
	for _, value := range operationsAttr.Values {
		if value.T != goipp.TagEnum {
			return false
		}
		integer, ok := value.V.(goipp.Integer)
		if !ok {
			return false
		}
		if goipp.Op(integer) == operation {
			return true
		}
	}
	return false
}

func (s *Service) withPayloadSlot(queue string, req *goipp.Message, operation func() (*goipp.Message, error)) (*goipp.Message, error) {
	slot := s.payloadSlots[queue]
	if slot == nil {
		return operation()
	}
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
		return operation()
	default:
		resp := goipp.NewResponse(req.Version, goipp.StatusErrorBusy, req.RequestID)
		resp.Operation = responseOperationAttrs("too many concurrent document submissions")
		return resp, nil
	}
}

func (s *Service) handleMappedJobOperation(ctx context.Context, queue string, printer config.PrinterConfig, req *goipp.Message) (*goipp.Message, error) {
	job, lookupErr := s.lookupRequestJob(ctx, queue, req)
	if lookupErr != nil {
		if errors.Is(lookupErr, sql.ErrNoRows) {
			resp := goipp.NewResponse(req.Version, goipp.StatusErrorNotFound, req.RequestID)
			resp.Operation = responseOperationAttrs("unknown job")
			return resp, nil
		}
		return nil, lookupErr
	}
	if !job.HasUpstreamJobID || job.UpstreamJobID < 1 || job.State == store.StateReserved || job.State == store.StateUncertain {
		return protocolResponse(req, goipp.StatusErrorNotPossible, "job mapping is not yet available"), nil
	}
	op := goipp.Op(req.Code)
	if store.IsTerminalState(job.State) && op != goipp.OpGetJobAttributes {
		return protocolResponse(req, goipp.StatusErrorNotPossible, "job is already terminal"), nil
	}
	req.Operation = rewriteMappedJobOperation(req.Operation, job)
	req.Operation = rewriteOperationForUpstream(req.Operation, printer.UpstreamURI)
	req.Groups = nil
	resp, err := s.upstream.Do(ctx, printer.UpstreamURI, req, nil)
	persistCtx, cancelPersist := s.persistenceContext()
	defer cancelPersist()
	if err != nil {
		if op == goipp.OpCancelJob || op == goipp.OpCloseJob {
			_ = s.store.MarkUncertain(persistCtx, queue, job.ProxyJobID, err.Error())
		}
		return nil, err
	}
	if !ippSuccess(resp) {
		s.logger.Warn("mapped job operation rejected by upstream",
			"queue", queue,
			"operation", goipp.Op(req.Code).String(),
			"upstream_status", goipp.Status(resp.Code).String(),
			"status_message", statusMessage(resp),
		)
		rewriteResponseJob(resp, queue, job.ProxyJobID, s.proxyPrinterURI(queue))
		return resp, nil
	}
	rewriteResponseJob(resp, queue, job.ProxyJobID, s.proxyPrinterURI(queue))
	if op == goipp.OpCancelJob {
		if stateErr := s.store.MarkTerminal(persistCtx, queue, job.ProxyJobID, store.StateCanceled, ""); stateErr != nil {
			s.logger.Warn("record canceled job state failed", "queue", queue, "proxy_job_id", job.ProxyJobID, "error", stateErr)
		}
	} else {
		s.syncObservedJob(persistCtx, queue, job, resp.Job)
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
		attrs = append(attrs, "printer_uri", redactDumpString(value))
	}
	if value, ok := iattr.FirstString(req.Operation, "job-uri"); ok {
		attrs = append(attrs, "job_uri", redactDumpString(value))
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
	return orderOperationTargets(attrs)
}

func rewriteMappedJobOperation(attrs goipp.Attributes, job store.Job) goipp.Attributes {
	attrs = iattr.SetAttr(attrs, iattr.Integer("job-id", job.UpstreamJobID))
	// RFC 8011 identifies a Job using either job-uri or printer-uri plus
	// job-id. The configured printer-uri is added by
	// rewriteOperationForUpstream, so remove job-uri to avoid sending both
	// target forms. Some printers reject that ambiguous combination.
	return orderOperationTargets(iattr.DropAttrs(attrs, "job-uri"))
}

func orderOperationTargets(attrs goipp.Attributes) goipp.Attributes {
	ordered := make(goipp.Attributes, 0, len(attrs))
	for _, name := range []string{"attributes-charset", "attributes-natural-language", "printer-uri", "job-uri", "job-id"} {
		if attr, ok := iattr.Attr(attrs, name); ok {
			ordered = append(ordered, attr)
		}
	}
	for _, attr := range attrs {
		switch strings.ToLower(attr.Name) {
		case "attributes-charset", "attributes-natural-language", "printer-uri", "job-uri", "job-id":
			continue
		}
		ordered = append(ordered, attr)
	}
	return ordered
}

func rewriteResponseJob(resp *goipp.Message, queue string, proxyJobID int, printerURI string) {
	resp.Job = rewriteJobAttrs(resp.Job, proxyJobID, printerURI)
	if resp.Groups != nil {
		for index := range resp.Groups {
			if resp.Groups[index].Tag == goipp.TagJobGroup {
				resp.Groups[index].Attrs = rewriteJobAttrs(resp.Groups[index].Attrs, proxyJobID, printerURI)
			}
		}
	}
	_ = queue
}

func rewriteJobAttrs(attrs goipp.Attributes, proxyJobID int, printerURI string) goipp.Attributes {
	attrs = iattr.SetAttr(attrs, iattr.Integer("job-id", proxyJobID))
	attrs = iattr.SetAttr(attrs, iattr.URI("job-uri", proxyJobURI(printerURI, proxyJobID)))
	attrs = iattr.SetAttr(attrs, iattr.URI("job-printer-uri", printerURI))
	if attr, ok := iattr.Attr(attrs, "job-state"); !ok || !validJobStateAttribute(attr) {
		attrs = iattr.SetAttr(attrs, goipp.MakeAttribute("job-state", goipp.TagEnum, goipp.Integer(3)))
	}
	if attr, ok := iattr.Attr(attrs, "job-state-reasons"); !ok || !validJobStateReasonsAttribute(attr) {
		attrs = iattr.SetAttr(attrs, iattr.Keyword("job-state-reasons", "none"))
	}
	// Upstream relative clocks describe a different Printer object. Dropping
	// them is safer than presenting internally inconsistent proxy job data.
	return iattr.DropAttrs(attrs,
		"time-at-creation", "time-at-processing", "time-at-completed",
		"date-time-at-creation", "date-time-at-processing", "date-time-at-completed",
		"job-printer-up-time")
}

func validJobStateAttribute(attr goipp.Attribute) bool {
	if len(attr.Values) != 1 || attr.Values[0].T != goipp.TagEnum {
		return false
	}
	state, ok := attr.Values[0].V.(goipp.Integer)
	return ok && state >= 3 && state <= 9
}

func validJobStateReasonsAttribute(attr goipp.Attribute) bool {
	if len(attr.Values) == 0 {
		return false
	}
	for _, value := range attr.Values {
		if value.T != goipp.TagKeyword {
			return false
		}
		reason, ok := value.V.(goipp.String)
		if !ok || strings.TrimSpace(string(reason)) == "" {
			return false
		}
	}
	return true
}

func requestedJobAttributes(operation goipp.Attributes) map[string]struct{} {
	attr, present := iattr.Attr(operation, "requested-attributes")
	if !present {
		return map[string]struct{}{"job-id": {}, "job-uri": {}}
	}
	wanted := make(map[string]struct{}, len(attr.Values))
	for _, value := range attr.Values {
		if text, ok := value.V.(goipp.String); ok {
			wanted[strings.ToLower(strings.TrimSpace(string(text)))] = struct{}{}
		}
	}
	return wanted
}

func projectJobAttributes(attrs goipp.Attributes, wanted map[string]struct{}) goipp.Attributes {
	if _, all := wanted["all"]; all {
		return attrs
	}
	projected := make(goipp.Attributes, 0, len(attrs))
	for _, attr := range attrs {
		if requestedJobAttribute(wanted, strings.ToLower(attr.Name)) {
			projected = append(projected, attr)
		}
	}
	return projected
}

func requestedJobAttribute(wanted map[string]struct{}, name string) bool {
	if _, keep := wanted[name]; keep {
		return true
	}
	if _, description := wanted["job-description"]; description {
		if _, keep := jobDescriptionAttributes[name]; keep {
			return true
		}
	}
	if _, template := wanted["job-template"]; template {
		if _, keep := jobTemplateAttributes[name]; keep {
			return true
		}
	}
	return false
}

var jobDescriptionAttributes = map[string]struct{}{
	"job-account-id":                  {},
	"job-accounting-impressions":      {},
	"job-accounting-sheets":           {},
	"job-accounting-sheets-completed": {},
	"job-accounting-user-id":          {},
	"job-cancel-after":                {},
	"job-completed-with-errors":       {},
	"job-document-access-errors":      {},
	"job-error-action":                {},
	"job-id":                          {},
	"job-impressions":                 {},
	"job-impressions-completed":       {},
	"job-k-octets":                    {},
	"job-k-octets-processed":          {},
	"job-media-sheets":                {},
	"job-media-sheets-completed":      {},
	"job-message-from-operator":       {},
	"job-more-info":                   {},
	"job-name":                        {},
	"job-originating-user-name":       {},
	"job-pages-completed":             {},
	"job-printer-up-time":             {},
	"job-printer-uri":                 {},
	"job-save-disposition":            {},
	"job-state":                       {},
	"job-state-message":               {},
	"job-state-reasons":               {},
	"job-uri":                         {},
	"job-uuid":                        {},
	"time-at-completed":               {},
	"time-at-creation":                {},
	"time-at-processing":              {},
	"date-time-at-completed":          {},
	"date-time-at-creation":           {},
	"date-time-at-processing":         {},
}

var jobTemplateAttributes = map[string]struct{}{
	"copies":                           {},
	"cover-back":                       {},
	"cover-front":                      {},
	"feed-orientation":                 {},
	"finishings":                       {},
	"finishings-col":                   {},
	"imposition-template":              {},
	"insert-sheet":                     {},
	"job-hold-until":                   {},
	"job-hold-until-time":              {},
	"job-priority":                     {},
	"job-sheets":                       {},
	"media":                            {},
	"media-col":                        {},
	"media-color":                      {},
	"media-source":                     {},
	"media-type":                       {},
	"media-weight":                     {},
	"multiple-document-handling":       {},
	"number-up":                        {},
	"orientation-requested":            {},
	"output-bin":                       {},
	"output-mode":                      {},
	"page-delivery":                    {},
	"page-ranges":                      {},
	"pages-per-subset":                 {},
	"presentation-direction-number-up": {},
	"print-color-mode":                 {},
	"print-content-optimize":           {},
	"print-quality":                    {},
	"print-scaling":                    {},
	"printer-resolution":               {},
	"sides":                            {},
}

func (s *Service) syncObservedJob(ctx context.Context, queue string, job store.Job, attrs goipp.Attributes) {
	state, ok := iattr.FirstInt(attrs, "job-state")
	if !ok {
		return
	}
	labels := map[int]string{3: "pending", 4: "pending-held", 5: "processing", 6: "processing-stopped", 7: "canceled", 8: "aborted", 9: "completed"}
	label, known := labels[state]
	if !known {
		label = strconv.Itoa(state)
	}
	var err error
	if state >= 7 && state <= 9 {
		err = s.store.MarkTerminal(ctx, queue, job.ProxyJobID, label, "")
	} else {
		err = s.store.UpdateObserved(ctx, queue, job.ProxyJobID, label, "")
	}
	if err != nil && !errors.Is(err, store.ErrInvalidTransition) {
		s.logger.Warn("update observed job state failed", "queue", queue, "proxy_job_id", job.ProxyJobID, "job_state", label, "error", err)
	}
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

func (s *Service) persistenceContext() (context.Context, context.CancelFunc) {
	parent := s.lifecycleCtx
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, persistenceTimeout)
}

func shapeIPPResponse(resp *goipp.Message) {
	if resp == nil {
		return
	}
	shape := func(attrs goipp.Attributes) goipp.Attributes {
		attrs = iattr.DropAttrs(attrs, "attributes-charset", "attributes-natural-language")
		return append(responseOperationAttrs(""), attrs...)
	}

	groups := resp.Groups
	if groups == nil {
		groups = goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: resp.Operation}}
		appendNamed := func(tag goipp.Tag, attrs goipp.Attributes) {
			if attrs != nil {
				groups = append(groups, goipp.Group{Tag: tag, Attrs: attrs})
			}
		}
		appendNamed(goipp.TagUnsupportedGroup, resp.Unsupported)
		appendNamed(goipp.TagJobGroup, resp.Job)
		appendNamed(goipp.TagPrinterGroup, resp.Printer)
		appendNamed(goipp.TagSubscriptionGroup, resp.Subscription)
		appendNamed(goipp.TagEventNotificationGroup, resp.EventNotification)
		appendNamed(goipp.TagResourceGroup, resp.Resource)
		appendNamed(goipp.TagDocumentGroup, resp.Document)
		appendNamed(goipp.TagSystemGroup, resp.System)
	}

	var operation, unsupported goipp.Attributes
	objects := make(goipp.Groups, 0, len(groups))
	objectIndex := make(map[goipp.Tag]int)
	for _, group := range groups {
		switch group.Tag {
		case goipp.TagOperationGroup:
			operation = append(operation, group.Attrs...)
		case goipp.TagUnsupportedGroup:
			unsupported = append(unsupported, group.Attrs...)
		case goipp.TagJobGroup:
			// Get-Jobs deliberately repeats this group once per returned Job.
			objects = append(objects, group)
		default:
			if index, exists := objectIndex[group.Tag]; exists {
				objects[index].Attrs = append(objects[index].Attrs, group.Attrs...)
				continue
			}
			objectIndex[group.Tag] = len(objects)
			objects = append(objects, group)
		}
	}
	if operation == nil {
		operation = resp.Operation
	}
	if unsupported == nil {
		unsupported = resp.Unsupported
	}
	operation = shape(operation)
	resp.Operation = operation
	resp.Unsupported = unsupported
	canonical := goipp.Groups{{Tag: goipp.TagOperationGroup, Attrs: operation}}
	if unsupported != nil {
		canonical = append(canonical, goipp.Group{Tag: goipp.TagUnsupportedGroup, Attrs: unsupported})
	}
	resp.Groups = append(canonical, objects...)
}

func writeIPP(w http.ResponseWriter, msg *goipp.Message) {
	w.Header().Set("content-type", goipp.ContentType)
	w.Header().Set("cache-control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_ = msg.Encode(w)
}

func (s *Service) writeIPPProtocolError(w http.ResponseWriter, version goipp.Version, requestID uint32, protocolErr *ProtocolError) {
	resp := goipp.NewResponse(version, protocolErr.Status, requestID)
	resp.Operation = responseOperationAttrs(protocolErr.Message)
	resp.Unsupported = protocolErr.Unsupported.DeepCopy()
	writeIPP(w, resp)
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
