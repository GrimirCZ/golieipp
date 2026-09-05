package proxy

import (
	"time"

	"github.com/OpenPrinting/goipp"
	"github.com/google/uuid"
	"github.com/grimir/golieipp/internal/ipp"
)

// PrinterIdentityMetadata contains the proxy-owned identity and clock values
// advertised in a Get-Printer-Attributes response. Uptime and
// ConfigChangeTime use the same service-start epoch, as required by IPP.
type PrinterIdentityMetadata struct {
	UUID                 string
	Uptime               int
	ConfigChangeTime     int
	ConfigChangeDateTime time.Time
}

// proxyPrinterUUID returns an RFC 4122 version 5 UUID in IPP's URI form.
func proxyPrinterUUID(proxyURI string) string {
	// uuid.NewSHA1 implements RFC 4122's version 5 (SHA-1 name-based) UUID.
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(proxyURI)).URN()
}

func defaultPrinterIdentityMetadata(proxyURI string) PrinterIdentityMetadata {
	return PrinterIdentityMetadata{
		UUID:                 proxyPrinterUUID(proxyURI),
		Uptime:               1,
		ConfigChangeTime:     1,
		ConfigChangeDateTime: time.Unix(0, 0).UTC(),
	}
}

func (s *Service) proxyPrinterIdentity(queue string) PrinterIdentityMetadata {
	proxyURI := s.proxyPrinterURI(queue)
	identity := defaultPrinterIdentityMetadata(proxyURI)
	s.mu.RLock()
	defer s.mu.RUnlock()

	startedAt := s.startedAt
	if startedAt.IsZero() {
		// NewService always initializes startedAt. The fallback keeps a manually
		// constructed Service deterministic in package-level tests and has no
		// effect on production instances.
		startedAt = time.Unix(0, 0)
	}
	configChangedAt := s.configChangedAt[queue]
	if configChangedAt.IsZero() {
		configChangedAt = startedAt
	}

	uptime := int(time.Since(startedAt) / time.Second)
	if uptime < 1 {
		uptime = 1
	}
	changeTime := int(configChangedAt.Sub(startedAt) / time.Second)
	if changeTime < 1 {
		changeTime = 1
	}
	if changeTime > uptime {
		changeTime = uptime
	}

	identity.Uptime = uptime
	identity.ConfigChangeTime = changeTime
	identity.ConfigChangeDateTime = configChangedAt.UTC()
	return identity
}

func proxyPrinterIdentityAttributes(identity PrinterIdentityMetadata) goipp.Attributes {
	if identity.UUID == "" {
		identity.UUID = proxyPrinterUUID("")
	}
	return goipp.Attributes{
		ipp.URI("printer-uuid", identity.UUID),
		ipp.Integer("printer-up-time", identity.Uptime),
		ipp.Integer("printer-config-change-time", identity.ConfigChangeTime),
		goipp.MakeAttribute("printer-config-change-date-time", goipp.TagDateTime, goipp.Time{Time: identity.ConfigChangeDateTime}),
	}
}
