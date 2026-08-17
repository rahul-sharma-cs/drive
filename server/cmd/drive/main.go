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
	"github.com/rahul-sharma-cs/drive/server/internal/gc"
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

	// The collector runs in-process: this service is resident, so an hourly
	// ticker is the whole scheduler. One pass runs at startup because the ticker
	// alone would leave a restarted process collecting nothing for an hour --
	// and a deploy that restarts often would mean it never runs at all.
	collector := gc.New(pool, s3Client, cfg.S3Bucket, logger)
	if err := collector.RunOnce(bootCtx); err != nil {
		logger.Error("startup garbage collection pass failed", "error", err)
	}
	go collector.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.New(cfg, pool, logger, mailSender(cfg, logger), s3Client, presign).Routes(),
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

// mailSender picks the outbound path. The API key's presence is the switch:
// with one, mail goes over Resend's HTTPS API, which is the only way off a
// Railway Hobby dyno (outbound SMTP is blocked there); without one, it is plain
// SMTP to a local Mailpit, which is what makes the verification loop readable
// in dev and in the test stack.
func mailSender(cfg *config.Config, logger *slog.Logger) mail.Sender {
	if cfg.UseResend() {
		logger.Info("mail: sending over the Resend API", "from", cfg.MailFrom)
		return mail.NewResendSender(cfg.ResendKey, cfg.MailFrom)
	}
	logger.Info("mail: sending over SMTP", "addr", cfg.SMTPAddr)
	return &mail.SMTPSender{Addr: cfg.SMTPAddr, From: cfg.MailFrom}
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
