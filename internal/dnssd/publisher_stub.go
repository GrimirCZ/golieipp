//go:build !linux || !avahi

package dnssd

import (
	"log/slog"

	"github.com/grimir/golieipp/internal/config"
)

// newPlatformPublisher is intentionally a no-op on non-Linux hosts and on
// Linux builds without the avahi tag. The returned publisher keeps lifecycle
// calls safe while exposing degraded state instead of claiming network
// publication.
func newPlatformPublisher(cfg config.DNSSDConfig, logger *slog.Logger) Publisher {
	publisher := NewStubPublisher()
	publisher.logger = logger
	// The platform stub cannot announce on the network. Keep that distinction
	// visible to health/readiness consumers instead of claiming publication.
	publisher.available = false
	if cfg.Mode == config.DNSModeOff {
		publisher.disabled = true
	}
	return publisher
}
