//go:build linux && avahi

package proxy

import (
	"context"

	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/dnssd"
)

func init() {
	registerDiagnosticPlugin(mdnsDiagnosticPlugin{})
}

func (mdnsDiagnosticPlugin) AddReadiness(result *readinessResponse, service *Service) {
	result.MDNS = &mdnsReadiness{State: service.mdnsStateLocked()}
}

// mdnsStateLocked returns a deliberately small aggregate for readiness. It
// reports discovery state without exposing publication names, TXT data, or
// daemon details through the public endpoint. The caller must hold s.mu for
// reading or writing.
func (s *Service) mdnsStateLocked() string {
	if s.cfg.DNSSD.Mode == config.DNSModeOff {
		return string(dnssd.StateDisabled)
	}

	configured := false
	published := false
	degraded := false
	for queue, printer := range s.cfg.Printers {
		if !printer.DNSSD {
			continue
		}
		configured = true
		health := s.queueHealth[queue]
		switch dnssd.State(health.DNSState) {
		case dnssd.StateDegraded:
			degraded = true
		case dnssd.StatePublished:
			published = true
		}
	}
	if !configured {
		return string(dnssd.StateDisabled)
	}
	if degraded {
		return string(dnssd.StateDegraded)
	}
	if published {
		return string(dnssd.StatePublished)
	}
	return string(dnssd.StateUnpublished)
}

type mdnsDiagnosticPlugin struct{}

func (mdnsDiagnosticPlugin) Name() string { return "mdns" }

func (mdnsDiagnosticPlugin) Dump(_ context.Context, snapshot diagnosticSnapshot) ([]diagnosticSection, error) {
	queues := make([]map[string]any, 0, len(snapshot.Queues))
	for _, queue := range snapshot.Queues {
		publisherState := dnssd.DebugState{Backend: "none", Availability: "not_configured"}
		if publisher, ok := queue.Publisher.(dnssd.DiagnosticPublisher); ok {
			publisherState = publisher.DebugState()
		}
		publication := map[string]any{
			"present":       publisherState.HasPublication,
			"registrations": dnsRegistrationDump(publisherState.Publication.Records),
		}
		if publisherState.HasPublication {
			publication["name"] = publisherState.Publication.Name
			publication["hostname"] = publisherState.Publication.Input.Hostname
			publication["port"] = publisherState.Publication.Input.Port
			publication["ipps"] = publisherState.Publication.Input.IPPS
			publication["interface"] = publisherState.Publication.Input.Interface
			publication["geo_location"] = publisherState.Publication.Input.GeoLocation
		}
		queues = append(queues, map[string]any{
			"queue":                   queue.Name,
			"configured":              queue.Printer.DNSSD,
			"ipp_everywhere_eligible": queue.Health.IPPEligible,
			"retrying":                queue.DNSRetrying,
			"state":                   queue.Health.DNSState,
			"degraded":                queue.Health.DNSDegraded,
			"name":                    queue.Health.DNSName,
			"last_attempt":            queue.Health.DNSLastAttempt,
			"last_published":          queue.Health.DNSLastPublished,
			"last_error":              diagnosticError(queue.Health.DNSLastError, queue.Printer.UpstreamURI),
			"attempts":                queue.Health.DNSAttempts,
			"publisher_backend":       publisherState.Backend,
			"publisher_availability":  publisherState.Availability,
			"entry_group":             publisherState.EntryGroup,
			"entry_group_state":       publisherState.EntryGroupState,
			"entry_group_state_error": publisherState.EntryGroupStateErr,
			"closed":                  publisherState.Closed,
			"publication":             publication,
		})
	}
	return []diagnosticSection{{
		Name: "mdns",
		Value: map[string]any{
			"mode":      snapshot.DNSMode,
			"hostname":  snapshot.DNSHostname,
			"interface": snapshot.DNSInterface,
			"state":     mdnsStateFromSnapshot(snapshot),
			"queues":    queues,
		},
	}}, nil
}

func mdnsStateFromSnapshot(snapshot diagnosticSnapshot) string {
	if snapshot.DNSMode == config.DNSModeOff {
		return string(dnssd.StateDisabled)
	}
	configured := false
	published := false
	degraded := false
	for _, queue := range snapshot.Queues {
		if !queue.Printer.DNSSD {
			continue
		}
		configured = true
		switch dnssd.State(queue.Health.DNSState) {
		case dnssd.StateDegraded:
			degraded = true
		case dnssd.StatePublished:
			published = true
		}
	}
	if !configured {
		return string(dnssd.StateDisabled)
	}
	if degraded {
		return string(dnssd.StateDegraded)
	}
	if published {
		return string(dnssd.StatePublished)
	}
	return string(dnssd.StateUnpublished)
}

func dnsRegistrationDump(records []dnssd.ServiceRecord) []map[string]any {
	dump := make([]map[string]any, 0, len(records))
	for _, record := range records {
		dump = append(dump, map[string]any{
			"name":         record.Name,
			"hostname":     record.Hostname,
			"port":         record.Port,
			"type":         record.Type,
			"subtype":      record.Subtype,
			"full_type":    record.FullType(),
			"txt":          record.TXT,
			"geo_location": record.GeoLocation,
		})
	}
	return dump
}
