package proxy

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/dnssd"
	"github.com/grimir/golieipp/internal/store"
)

// diagnosticSection is the unit emitted by a diagnostic plugin. Optional
// sections are registered by build-tagged files, so a binary cannot claim to
// report state for a subsystem that was not compiled into it.
type diagnosticSection struct {
	Name  string
	Value any
}

type diagnosticPlugin interface {
	Name() string
	Dump(context.Context, diagnosticSnapshot) ([]diagnosticSection, error)
}

// readinessContributor is an optional diagnostic-plugin extension for the
// deliberately small public readiness response. A plugin contributes only
// when its implementation is compiled in and registered.
type readinessContributor interface {
	AddReadiness(*readinessResponse, *Service)
}

type diagnosticSnapshot struct {
	Now               time.Time
	StartedAt         time.Time
	NextRequestID     uint64
	Closing           bool
	UpstreamTimeout   time.Duration
	ProbeTimeout      time.Duration
	MaxResponseBytes  int64
	EffectiveConfig   map[string]any
	DNSMode           string
	DNSHostname       string
	DNSInterface      string
	Queues            []diagnosticQueue
	JobSummary        store.JobSummary
	JobSummaryError   string
	JobStoreAvailable bool
}

type diagnosticQueue struct {
	Name                 string
	Printer              config.PrinterConfig
	PublicURI            string
	Active               bool
	Stale                bool
	RefreshInFlight      bool
	ConfigChangedAt      time.Time
	Health               queueHealth
	PayloadJobsInFlight  int
	PayloadJobCapacity   int
	DNSRetrying          bool
	UpstreamCapabilities goipp.Attributes
	ClientCapabilities   goipp.Attributes
	Profiles             CapabilityProfiles
	Publisher            dnssd.Publisher
}

type applicationDiagnosticPlugin struct{}

func (applicationDiagnosticPlugin) Name() string { return "application" }

func (applicationDiagnosticPlugin) Dump(_ context.Context, snapshot diagnosticSnapshot) ([]diagnosticSection, error) {
	sections := []diagnosticSection{
		{
			Name: "application",
			Value: map[string]any{
				"snapshot_at":                 snapshot.Now,
				"started_at":                  snapshot.StartedAt,
				"next_request_id":             snapshot.NextRequestID,
				"closing":                     snapshot.Closing,
				"queue_count":                 len(snapshot.Queues),
				"job_store_present":           snapshot.JobStoreAvailable,
				"upstream_timeout":            snapshot.UpstreamTimeout,
				"upstream_probe_timeout":      snapshot.ProbeTimeout,
				"upstream_max_response_bytes": snapshot.MaxResponseBytes,
			},
		},
		{
			Name:  "effective_config",
			Value: snapshot.EffectiveConfig,
		},
	}

	jobState := map[string]any{
		"available": snapshot.JobStoreAvailable,
		"total":     snapshot.JobSummary.Total,
		"by_queue":  snapshot.JobSummary.ByQueue,
		"by_state":  snapshot.JobSummary.ByState,
	}
	if snapshot.JobSummaryError != "" {
		jobState["error"] = snapshot.JobSummaryError
	}
	sections = append(sections, diagnosticSection{Name: "job_registry", Value: jobState})

	for _, queue := range snapshot.Queues {
		health := queue.Health
		queueState := map[string]any{
			"queue":                   queue.Name,
			"upstream_uri":            redactDumpString(queue.Printer.UpstreamURI),
			"public_uri":              queue.PublicURI,
			"display_name":            queue.Printer.DisplayName,
			"location":                queue.Printer.Location,
			"optional":                queue.Printer.Optional,
			"airprint_mode":           queue.Printer.AirPrintMode,
			"ipp_everywhere_mode":     queue.Printer.IPPEverywhereMode,
			"active":                  queue.Active,
			"stale":                   queue.Stale,
			"refresh_in_flight":       queue.RefreshInFlight,
			"config_changed_at":       queue.ConfigChangedAt,
			"last_success":            health.LastSuccess,
			"last_attempt":            health.LastAttempt,
			"last_error":              diagnosticError(health.LastError, queue.Printer.UpstreamURI),
			"ipp_everywhere_eligible": health.IPPEligible,
			"profiles":                diagnosticProfiles(queue.Profiles),
			"profile_warnings":        health.ProfileWarnings,
			"payload_jobs_in_flight":  queue.PayloadJobsInFlight,
			"payload_job_capacity":    queue.PayloadJobCapacity,
		}
		sections = append(sections, diagnosticSection{Name: "queue", Value: queueState})
		sections = append(sections, diagnosticSection{
			Name: "capabilities",
			Value: map[string]any{
				"queue":    queue.Name,
				"upstream": dumpAttributes(queue.UpstreamCapabilities),
				"client":   dumpAttributes(queue.ClientCapabilities),
			},
		})
	}
	return sections, nil
}

// diagnosticPluginRegistry is populated by optional providers during package
// initialization. The map makes the plugin name the identity of a provider,
// so a duplicate name fails early instead of producing duplicate sections.
// Registration is complete before application code can call DumpState; after
// initialization the map is read-only.
var diagnosticPluginRegistry = make(map[string]diagnosticPlugin)

func registerDiagnosticPlugin(plugin diagnosticPlugin) {
	if plugin == nil {
		panic("cannot register a nil diagnostic plugin")
	}
	name := plugin.Name()
	if name == "" {
		panic("cannot register a diagnostic plugin without a name")
	}
	if _, exists := diagnosticPluginRegistry[name]; exists {
		panic("diagnostic plugin already registered: " + name)
	}
	diagnosticPluginRegistry[name] = plugin
}

func diagnosticPlugins() []diagnosticPlugin {
	return orderedDiagnosticPlugins(applicationDiagnosticPlugin{}, diagnosticPluginRegistry)
}

func orderedDiagnosticPlugins(core diagnosticPlugin, registry map[string]diagnosticPlugin) []diagnosticPlugin {
	plugins := []diagnosticPlugin{core}
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		plugins = append(plugins, registry[name])
	}
	return plugins
}

// DumpState emits all diagnostic sections compiled into this binary. The
// operation is on-demand and is intended for a local operator signal, not for
// an HTTP health check.
func (s *Service) DumpState() {
	snapshot := s.captureDiagnosticSnapshot()
	for _, plugin := range diagnosticPlugins() {
		sections, err := plugin.Dump(context.Background(), snapshot)
		if err != nil {
			s.logger.Error("diagnostic section failed", "section", plugin.Name(), "error", err)
			continue
		}
		for _, section := range sections {
			s.logger.Info("diagnostic state", "section", section.Name, "state", section.Value)
		}
	}
}

func (s *Service) captureDiagnosticSnapshot() diagnosticSnapshot {
	now := time.Now().UTC()
	snapshot := diagnosticSnapshot{
		Now:             now,
		StartedAt:       s.startedAt,
		NextRequestID:   s.nextID.Load(),
		EffectiveConfig: effectiveConfigDump(s.cfg),
	}
	if s.upstream != nil {
		snapshot.ProbeTimeout = s.upstream.ProbeTimeout
		snapshot.MaxResponseBytes = s.upstream.MaxResponseBytes
		if s.upstream.HTTP != nil {
			snapshot.UpstreamTimeout = s.upstream.HTTP.Timeout
		}
	}

	s.mu.RLock()
	snapshot.Closing = s.closing
	snapshot.DNSMode = s.cfg.DNSSD.Mode
	snapshot.DNSHostname = s.cfg.DNSSD.Hostname
	snapshot.DNSInterface = s.cfg.DNSSD.Interface
	snapshot.Queues = make([]diagnosticQueue, 0, len(s.cfg.Printers))
	for queue, printer := range s.cfg.Printers {
		active := false
		if _, ok := s.capabilities[queue]; ok {
			active = true
		}
		interval := printer.RefreshInterval
		if interval <= 0 {
			interval = 5 * time.Minute
		}
		health := s.queueHealth[queue]
		profiles := s.profilesLocked(queue, printer, s.capabilities[queue])
		payloadJobsInFlight, payloadJobCapacity := 0, 0
		if slot := s.payloadSlots[queue]; slot != nil {
			payloadJobsInFlight = len(slot)
			payloadJobCapacity = cap(slot)
		}
		queueSnapshot := diagnosticQueue{
			Name:                queue,
			Printer:             printer,
			PublicURI:           s.proxyPrinterURI(queue),
			Active:              active,
			Stale:               active && !health.LastSuccess.IsZero() && now.Sub(health.LastSuccess) > 2*interval,
			RefreshInFlight:     s.refreshing[queue],
			ConfigChangedAt:     s.configChangedAt[queue],
			Health:              health,
			Profiles:            profiles,
			PayloadJobsInFlight: payloadJobsInFlight,
			PayloadJobCapacity:  payloadJobCapacity,
			DNSRetrying:         s.dnsRetrying[queue],
			Publisher:           s.publishers[queue],
		}
		if attrs, ok := s.capabilities[queue]; ok {
			queueSnapshot.UpstreamCapabilities = attrs.Clone()
		}
		snapshot.Queues = append(snapshot.Queues, queueSnapshot)
	}
	jobStore := s.store
	s.mu.RUnlock()

	sort.Slice(snapshot.Queues, func(i, j int) bool { return snapshot.Queues[i].Name < snapshot.Queues[j].Name })
	for index := range snapshot.Queues {
		queue := &snapshot.Queues[index]
		if len(queue.UpstreamCapabilities) > 0 {
			queue.ClientCapabilities = s.clientCapabilities(queue.Name, queue.Printer, queue.UpstreamCapabilities)
		}
	}
	if jobStore == nil {
		snapshot.JobSummaryError = "job store is unavailable"
		return snapshot
	}
	snapshot.JobStoreAvailable = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	snapshot.JobSummary, err = jobStore.SummarizeJobs(ctx)
	if err != nil {
		snapshot.JobSummaryError = err.Error()
	}
	return snapshot
}

func effectiveConfigDump(cfg *config.Config) map[string]any {
	if cfg == nil {
		return nil
	}
	printers := make(map[string]any, len(cfg.Printers))
	for queue, printer := range cfg.Printers {
		printers[queue] = map[string]any{
			"upstream_uri":        redactDumpString(printer.UpstreamURI),
			"display_name":        printer.DisplayName,
			"location":            printer.Location,
			"optional":            printer.Optional,
			"ipp_everywhere_mode": printer.IPPEverywhereMode,
			"airprint_mode":       printer.AirPrintMode,
			"dns_sd":              printer.DNSSD,
			"geo_location":        printer.GeoLocation,
			"refresh_interval":    printer.RefreshInterval,
			"policy":              policyConfigDump(printer.Policy),
			"passthrough":         passthroughConfigDump(printer.Passthrough),
		}
	}
	return map[string]any{
		"listen": map[string]any{
			"addr":            cfg.Listen.Addr,
			"public_base_url": redactDumpString(cfg.Listen.PublicBaseURL),
		},
		"storage": map[string]any{
			"sqlite_path": cfg.Storage.SQLitePath,
		},
		"defaults": defaultsConfigDump(cfg.Defaults),
		"dns_sd": map[string]any{
			"mode":            cfg.DNSSD.Mode,
			"hostname":        cfg.DNSSD.Hostname,
			"interface":       cfg.DNSSD.Interface,
			"allowed_aliases": append([]string(nil), cfg.DNSSD.AllowedAliases...),
		},
		"printers": printers,
	}
}

func defaultsConfigDump(defaults config.DefaultsConfig) map[string]any {
	return map[string]any{
		"max_envelope_bytes":                    defaults.MaxEnvelopeBytes,
		"max_document_bytes":                    defaults.MaxDocumentBytes,
		"max_upstream_response_bytes":           defaults.MaxUpstreamResponseBytes,
		"max_concurrent_payload_jobs_per_queue": defaults.MaxConcurrentPayloadJobsPerQueue,
		"job_retention":                         defaults.JobRetention,
	}
}

func policyConfigDump(policy config.PolicyConfig) map[string]any {
	return map[string]any{
		"media":            policy.Media,
		"media_supported":  policy.MediaSupported,
		"media_default":    policy.MediaDefault,
		"media_type":       policy.MediaType,
		"print_color_mode": policy.PrintColorMode,
		"media_source":     optionalConfigString(policy.MediaSource),
		"print_scaling":    optionalConfigString(policy.PrintScaling),
		"use_media_col":    policy.UseMediaCol,
		"fidelity_mode":    policy.FidelityMode,
	}
}

func passthroughConfigDump(passthrough config.PassthroughConfig) map[string]any {
	return map[string]any{
		"allow_unknown_attributes": passthrough.AllowUnknownAttributes,
		"preserve_job_attrs":       passthrough.PreserveJobAttrs,
		"drop_vendor_attrs":        passthrough.DropVendorAttrs,
	}
}

func optionalConfigString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func diagnosticProfiles(profiles CapabilityProfiles) map[string]any {
	return map[string]any{
		"ordinary":       diagnosticProfile(profiles.Ordinary),
		"airprint":       diagnosticProfile(profiles.AirPrint),
		"ipp_everywhere": diagnosticProfile(profiles.IPPEverywhere),
	}
}

func diagnosticProfile(profile CapabilityProfile) map[string]any {
	return map[string]any{
		"enabled":  profile.Enabled,
		"ready":    profile.Ready,
		"reason":   profile.Reason,
		"warnings": append([]string(nil), profile.Warnings...),
	}
}

func diagnosticError(message, upstreamURI string) string {
	if message == "" {
		return ""
	}
	return safeProbeError(errors.New(message), upstreamURI)
}
