//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/grimir/golieipp/internal/proxy"
)

func installDiagnosticSignal(ctx context.Context, svc *proxy.Service, logger *slog.Logger) func() {
	dumpSignals := make(chan os.Signal, 1)
	signal.Notify(dumpSignals, syscall.SIGUSR1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case sig := <-dumpSignals:
				logger.Info("received application diagnostic dump signal", "signal", sig.String())
				svc.DumpState()
			}
		}
	}()
	return func() { signal.Stop(dumpSignals) }
}
