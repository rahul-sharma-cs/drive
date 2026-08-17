// Package config loads Drive's runtime configuration from the environment.
//
// Every value has a dev default so a bare `go run ./cmd/drive` works; real
// values come from .env, which is never tracked.
package config

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// dialTimeout bounds every boot-time reachability probe.
const dialTimeout = 5 * time.Second

const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
)

// DefaultS3Region is Garage's s3_api.s3_region from garage.toml; a mismatch
// signs requests the store then rejects with SignatureDoesNotMatch. R2 wants
// "auto", set through DRIVE_S3_REGION.
const DefaultS3Region = "garage"

const (
	// minPartSize is S3's floor for a non-final multipart part.
	minPartSize = 5 * MiB
	// garageBlockSize mirrors garage.toml's block_size; parts must be a
	// multiple of it.
	garageBlockSize = 10 * MiB
)

// Config is the server's fully resolved configuration.
type Config struct {
	Addr            string
	BaseURL         string
	DBDSN           string
	S3Endpoint      string
	S3Bucket        string
	S3AccessKey     string
	S3SecretKey     string
	S3Region        string
	SMTPAddr        string
	MailpitAPI      string
	PartSize        int64
	PresignTTL      time.Duration
	TokenPresignTTL time.Duration
	LogLevel        string
}

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	cfg := &Config{
		Addr:        env("DRIVE_ADDR", ":8080"),
		BaseURL:     env("DRIVE_BASE_URL", "http://localhost:8080"),
		DBDSN:       env("DRIVE_DB_DSN", "postgres://drive:drive@localhost:55432/drive?sslmode=disable"),
		S3Endpoint:  env("DRIVE_S3_ENDPOINT", "http://localhost:3900"),
		S3Bucket:    env("DRIVE_S3_BUCKET", "drive-blobs"),
		S3AccessKey: env("DRIVE_S3_ACCESS_KEY", ""),
		S3SecretKey: env("DRIVE_S3_SECRET_KEY", ""),
		// Signing region. Garage's own s3_region locally; "auto" on R2.
		S3Region:   env("DRIVE_S3_REGION", DefaultS3Region),
		SMTPAddr:   env("DRIVE_SMTP_ADDR", "localhost:1025"),
		MailpitAPI: env("DRIVE_MAILPIT_API", "http://localhost:8025"),
		LogLevel:   env("DRIVE_LOG_LEVEL", "debug"),
	}

	var err error
	if cfg.PartSize, err = ParseSize(env("DRIVE_PART_SIZE", "100MiB")); err != nil {
		return nil, fmt.Errorf("DRIVE_PART_SIZE: %w", err)
	}
	if cfg.PresignTTL, err = time.ParseDuration(env("DRIVE_PRESIGN_TTL", "1h")); err != nil {
		return nil, fmt.Errorf("DRIVE_PRESIGN_TTL: %w", err)
	}
	if cfg.TokenPresignTTL, err = time.ParseDuration(env("DRIVE_TOKEN_PRESIGN_TTL", "10m")); err != nil {
		return nil, fmt.Errorf("DRIVE_TOKEN_PRESIGN_TTL: %w", err)
	}
	return cfg, nil
}

// Validate fails fast at boot, naming the offending variable.
func (c *Config) Validate() error {
	required := []struct {
		name  string
		value string
	}{
		{"DRIVE_ADDR", c.Addr},
		{"DRIVE_BASE_URL", c.BaseURL},
		{"DRIVE_DB_DSN", c.DBDSN},
		{"DRIVE_S3_ENDPOINT", c.S3Endpoint},
		{"DRIVE_S3_BUCKET", c.S3Bucket},
		{"DRIVE_SMTP_ADDR", c.SMTPAddr},
		{"DRIVE_MAILPIT_API", c.MailpitAPI},
	}
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			return fmt.Errorf("%s: must not be empty", r.name)
		}
	}

	if c.PartSize < minPartSize {
		return fmt.Errorf("DRIVE_PART_SIZE: %d is below S3's 5MiB minimum part size", c.PartSize)
	}
	if c.PartSize%garageBlockSize != 0 {
		return fmt.Errorf("DRIVE_PART_SIZE: %d is not a multiple of Garage's 10MiB block size", c.PartSize)
	}

	return nil
}

// ValidateRuntime checks that the services the config points at are actually
// reachable, naming the offending variable. It is separate from Validate so
// pure-value tests stay fast and offline.
//
// The "is this our database" check is not here: it is goose's marker table, and
// it lives in internal/db next to the migration run it guards.
func (c *Config) ValidateRuntime(ctx context.Context) error {
	if err := dialDSN(ctx, c.DBDSN); err != nil {
		return fmt.Errorf("DRIVE_DB_DSN: %w", err)
	}
	if err := reachHTTP(ctx, c.S3Endpoint); err != nil {
		return fmt.Errorf("DRIVE_S3_ENDPOINT: %w", err)
	}
	if err := dialTCP(ctx, c.SMTPAddr); err != nil {
		return fmt.Errorf("DRIVE_SMTP_ADDR: %w", err)
	}
	return nil
}

// dialDSN opens a TCP connection to the Postgres host in the DSN. It proves
// reachability without pulling a driver into this package; internal/db does the
// real connect and ping right after.
func dialDSN(ctx context.Context, dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("not a valid connection URL: %w", err)
	}
	host := u.Host
	if host == "" {
		return fmt.Errorf("no host in %q", dsn)
	}
	if u.Port() == "" {
		host = net.JoinHostPort(host, "5432")
	}
	return dialTCP(ctx, host)
}

func dialTCP(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port address: %w", err)
	}
	if host == "" {
		host = "localhost"
	}
	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	return conn.Close()
}

// reachHTTP proves the S3 endpoint answers. Garage replies 403 to an
// unauthenticated GET /, which is a perfectly good sign of life -- any HTTP
// status counts, only a transport failure does not.
func reachHTTP(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	client := &http.Client{Timeout: dialTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10))
	return nil
}

// ParseSize parses a byte size: a plain byte count ("104857600") or a value
// with a KiB/MiB/GiB suffix ("100MiB").
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("empty size")
	}

	unit := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"KIB", KiB}, {"MIB", MiB}, {"GIB", GiB}, {"B", 1}} {
		if strings.HasSuffix(strings.ToUpper(t), u.suffix) {
			unit = u.mult
			t = strings.TrimSpace(t[:len(t)-len(u.suffix)])
			break
		}
	}

	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: want bytes or a KiB/MiB/GiB value", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid size %q: must not be negative", s)
	}
	return n * unit, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
