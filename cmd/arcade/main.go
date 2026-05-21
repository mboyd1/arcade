package main

import (
	"context"
	"fmt"
	"net/http"
	_ "net/http/pprof" // diagnostic profiling, exposed only when ARCADE_PPROF_ADDR is set
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/bsv-blockchain/arcade/app"
	"github.com/bsv-blockchain/arcade/config"
	"github.com/bsv-blockchain/arcade/services"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "arcade",
		Short: "Arcade transaction management service",
		RunE:  run,
	}

	config.BindFlags(rootCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, _ []string) error {
	cfg, err := config.Load(cmd)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	logger := newLogger(cfg.LogLevel)
	defer func() { _ = logger.Sync() }()

	// Diagnostic pprof endpoint. Off by default; opt in by setting
	// ARCADE_PPROF_ADDR (e.g. "localhost:6060"). net/http/pprof registers
	// its handlers on http.DefaultServeMux, which the zero-value Server uses.
	if addr := os.Getenv("ARCADE_PPROF_ADDR"); addr != "" {
		go func() {
			logger.Info("pprof endpoint listening", zap.String("addr", addr))
			srv := &http.Server{Addr: addr, ReadHeaderTimeout: 5 * time.Second}
			if err := srv.ListenAndServe(); err != nil {
				logger.Warn("pprof endpoint stopped", zap.Error(err))
			}
		}()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deps, cleanup, err := app.Bootstrap(ctx, cfg, logger)
	if err != nil {
		return err
	}
	defer cleanup()

	svcs := app.BuildServices(deps)

	// Start health server for non-API modes (api-server serves /health on its own port)
	if cfg.Mode != "api-server" {
		hs := services.NewHealthServer(cfg.Health.Port, logger)
		hs.Start(ctx)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	var wg sync.WaitGroup
	errCh := make(chan error, len(svcs))

	for _, svc := range svcs {
		wg.Add(1)
		go func(s services.Service) {
			defer wg.Done()
			logger.Info("starting service", zap.String("service", s.Name()))
			if err := s.Start(ctx); err != nil {
				logger.Error("service failed", zap.String("service", s.Name()), zap.Error(err))
				errCh <- fmt.Errorf("service %s: %w", s.Name(), err)
			}
		}(svc)
	}

	select {
	case sig := <-sigCh:
		logger.Info("received signal, shutting down", zap.String("signal", sig.String()))
	case err := <-errCh:
		logger.Error("service error, shutting down", zap.Error(err))
	}

	cancel()

	for _, svc := range svcs {
		logger.Info("stopping service", zap.String("service", svc.Name()))
		if err := svc.Stop(); err != nil {
			logger.Error("error stopping service", zap.String("service", svc.Name()), zap.Error(err))
		}
	}

	wg.Wait()
	logger.Info("arcade stopped")
	return nil
}

func newLogger(level string) *zap.Logger {
	var zapLevel zapcore.Level
	switch level {
	case "debug":
		zapLevel = zap.DebugLevel
	case "warn":
		zapLevel = zap.WarnLevel
	case "error":
		zapLevel = zap.ErrorLevel
	default:
		zapLevel = zap.InfoLevel
	}

	cfg := zap.Config{
		Level:            zap.NewAtomicLevelAt(zapLevel),
		Encoding:         "json",
		OutputPaths:      []string{"stdout"},
		ErrorOutputPaths: []string{"stderr"},
		EncoderConfig:    zap.NewProductionEncoderConfig(),
	}

	logger, _ := cfg.Build()
	return logger
}
