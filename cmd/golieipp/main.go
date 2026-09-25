package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/proxy"
	"github.com/grimir/golieipp/internal/stats"
	"github.com/grimir/golieipp/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "stats" {
		if err := runStats(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "statistics:", err)
			os.Exit(1)
		}
		return
	}
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	debug := flag.Bool("debug", false, "enable debug logging")
	dumpCapabilities := flag.Bool("dump-printer-capabilities", false, "probe every configured printer and emit raw capabilities as JSON")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logOutput := os.Stdout
	if *dumpCapabilities {
		// stdout is reserved for the machine-readable report in dump mode.
		logOutput = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{Level: level}))
	cfg, err := config.LoadWithLogger(*configPath, logger)
	if err != nil {
		logger.Error("load config", "error", err)
		os.Exit(1)
	}
	logger.Debug("loaded config",
		"config", *configPath,
		"listen_addr", cfg.Listen.Addr,
		"public_base_url", cfg.Listen.PublicBaseURL,
		"printer_count", len(cfg.Printers),
	)
	if *dumpCapabilities {
		if err := proxy.DumpPrinterCapabilities(context.Background(), cfg, os.Stdout); err != nil {
			logger.Error("dump printer capabilities failed", "error", err)
			os.Exit(1)
		}
		return
	}

	jobStore, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		logger.Error("open job store", "error", err)
		os.Exit(1)
	}
	defer jobStore.Close()
	statsOptions := statisticsOptions(cfg.Statistics)
	var registryID string
	if statsOptions.Enabled {
		identityCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		registryID, err = jobStore.InstanceID(identityCtx)
		cancel()
		if err != nil {
			logger.Warn("statistics disabled: cannot identify job registry", "error", err)
			statsOptions.Enabled = false
		}
	}
	statistics := stats.New(statsOptions, logger)
	defer func() {
		if err := statistics.Close(); err != nil {
			logger.Warn("statistics shutdown", "error", err)
		}
	}()

	svc, err := proxy.NewService(cfg, jobStore, logger)
	if err != nil {
		logger.Error("create service", "error", err)
		os.Exit(1)
	}
	svc.SetStatistics(statistics, registryID)
	defer func() {
		if err := svc.Close(); err != nil {
			logger.Warn("close service", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopDiagnosticSignal := installDiagnosticSignal(ctx, svc, logger)
	defer stopDiagnosticSignal()

	if err := svc.RefreshAll(ctx); err != nil {
		logger.Error("refresh upstream capabilities", "error", err)
		os.Exit(1)
	}
	svc.LogPrinterURLs()
	go svc.StartRefreshLoop(ctx)
	go svc.StartMaintenanceLoop(ctx)

	server := &http.Server{
		Addr:              cfg.Listen.Addr,
		Handler:           svc.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", cfg.Listen.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("http server", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
}
