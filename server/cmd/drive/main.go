// Command drive is the Drive server: REST API plus the embedded SPA.
package main

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/rahul-sharma-cs/drive/server/internal/config"
	"github.com/rahul-sharma-cs/drive/server/web"
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

	dist, err := fs.Sub(web.Dist, "dist")
	if err != nil {
		fatal(err)
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(requestLogger(logger))
	r.Get("/healthz", healthz)
	// Everything the router does not match: the SPA owns non-/api GETs.
	r.NotFound(spaHandler(dist))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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

// healthz is the readiness probe make e2e and the integration harness wait on.
func healthz(w http.ResponseWriter, r *http.Request) {
	// Phase 1: return 200 only once the DB ping succeeds; there is no DB yet.
	loggerFrom(r.Context()).Debug("healthz")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// spaHandler serves the embedded SPA, falling back to index.html so client-side
// routes (/s/{token}, /verify, …) resolve. Unmatched /api paths stay JSON.
func spaHandler(dist fs.FS) http.HandlerFunc {
	files := http.FileServer(http.FS(dist))
	return func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"code":"not_found","message":"not found"}`))
			return
		}
		if _, err := fs.Stat(dist, strings.TrimPrefix(requestPath(r), "/")); err != nil {
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		files.ServeHTTP(w, r)
	}
}

func requestPath(r *http.Request) string {
	if r.URL.Path == "/" {
		return "/index.html"
	}
	return r.URL.Path
}

// requestLogger stamps the chi request id onto a logger stored in the request
// context, so every line a handler writes carries it.
func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			l := logger.With("request_id", middleware.GetReqID(r.Context()))
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			start := time.Now()

			next.ServeHTTP(ww, r.WithContext(withLogger(r.Context(), l)))

			l.Debug("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", ww.Status(),
				"bytes", ww.BytesWritten(),
				"duration_ms", time.Since(start).Milliseconds(),
			)
		})
	}
}

type loggerKey struct{}

func withLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, l)
}

// loggerFrom returns the request-scoped logger; handlers use it so their lines
// carry the request id.
func loggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
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
