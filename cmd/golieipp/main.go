package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grimir/golieipp/internal/config"
	"github.com/grimir/golieipp/internal/proxy"
	"github.com/grimir/golieipp/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML configuration")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	cfg, err := config.Load(*configPath)
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

	jobStore, err := store.Open(cfg.Storage.SQLitePath)
	if err != nil {
		logger.Error("open job store", "error", err)
		os.Exit(1)
	}
	defer jobStore.Close()

	svc, err := proxy.NewService(cfg, jobStore, logger)
	if err != nil {
		logger.Error("create service", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := svc.RefreshAll(ctx); err != nil {
		logger.Error("refresh upstream capabilities", "error", err)
		os.Exit(1)
	}
	svc.LogPrinterURLs()
	go svc.StartRefreshLoop(ctx)

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
