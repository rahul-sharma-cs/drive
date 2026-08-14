// Command drive is the Drive server: REST API plus the embedded SPA.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rahul-sharma-cs/drive/server/internal/api"
	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/internal/db"
	"github.com/rahul-sharma-cs/drive/server/internal/mail"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel(cfg.LogLevel)}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Boot validation: every external service the config names must answer
	// before we go any further, and the failure must name the variable.
	bootCtx, cancelBoot := context.WithTimeout(ctx, 30*time.Second)
	defer cancelBoot()
	if err := cfg.ValidateRuntime(bootCtx); err != nil {
		fatal(err)
	}

	pool, err := db.Connect(bootCtx, cfg.DBDSN)
	if err != nil {
		fatal(err)
	}
	defer pool.Close()

	if err := db.Migrate(bootCtx, pool); err != nil {
		fatal(err)
	}
	logger.Info("migrations applied")

	s3Client, presign, err := blob.New(bootCtx, cfg)
	if err != nil {
		fatal(err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(cfg, pool, logger, mail.NewSMTPSender(cfg.SMTPAddr), s3Client, presign).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("server listening", "addr", cfg.Addr, "base_url", cfg.BaseURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
		os.Exit(1)
	}
}

func logLevel(name string) slog.Level {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}
	return lvl
}

func fatal(err error) {
	slog.Error("startup failed", "error", err)
	os.Exit(1)
}
