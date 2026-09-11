//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package main

import (
	"context"
	"log/slog"

	"github.com/grimir/golieipp/internal/proxy"
)

// installDiagnosticSignal is a no-op on platforms without SIGUSR1. The
// diagnostic API remains available to in-process callers, while the command
// has no portable OS signal to bind to there.
func installDiagnosticSignal(context.Context, *proxy.Service, *slog.Logger) func() {
	return func() {}
}
