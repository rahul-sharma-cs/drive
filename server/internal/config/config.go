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
	ResendKey       string
	MailFrom        string
	PartSize        int64
	PresignTTL      time.Duration
	TokenPresignTTL time.Duration
	LogLevel        string
	Argon2Limit     int
	AuthRatePerMin  int
	SignupMode      string
	EmailDailyCap   int
	MaxFileSize     int64
	StorageCap      int64
	UserQuota       int64
}

// Signup modes. Only SignupOpen creates accounts; SignupInvite is accepted and
// behaves as SignupClosed until there is an invite system to back it.
const (
	SignupOpen   = "open"
	SignupInvite = "invite"
	SignupClosed = "closed"
)

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
		// Signing region: Garage's own s3_region locally, "auto" on R2.
		//
		// Deliberately no default here, unlike almost everything else in this
		// struct. A default would make the Validate check below unreachable --
		// the value could never be empty -- and the whole point of requiring it
		// is that a deployment which forgets it fails at boot instead of at
		// somebody's first upload, with SignatureDoesNotMatch. blob.New still
		// falls back to DefaultS3Region, which is what keeps the hand-built
		// Config literals in the test suites working.
		S3Region:   env("DRIVE_S3_REGION", ""),
		SMTPAddr:   env("DRIVE_SMTP_ADDR", "localhost:1025"),
		MailpitAPI: env("DRIVE_MAILPIT_API", "http://localhost:8025"),
		// Mail: the sender is chosen by ResendKey's presence. Blank means SMTP
		// to Mailpit, which is every local and test run; set means Resend's
		// HTTP API, which is the only path out of Railway's Hobby plan.
		// Blank MailFrom means the sender's own default (mail.DefaultFrom); a
		// deployment on a verified domain must set it, and no default can guess
		// what that domain is.
		ResendKey:  env("DRIVE_RESEND_KEY", ""),
		MailFrom:   env("DRIVE_MAIL_FROM", ""),
		SignupMode: env("DRIVE_SIGNUP_MODE", SignupOpen),
		// info by default, so an environment that sets nothing -- which is what
		// a deployment looks like -- is not writing a line per request. The dev
		// and test .env files ask for debug explicitly.
		LogLevel: env("DRIVE_LOG_LEVEL", "info"),
	}

	var err error
	// How many Argon2 operations may run at once. Each holds 19 MiB, so this is
	// a memory ceiling before it is a rate limit; tune it down on a small
	// container rather than removing it.
	if cfg.Argon2Limit, err = parseCount(env("DRIVE_ARGON2_LIMIT", "")); err != nil {
		return nil, fmt.Errorf("DRIVE_ARGON2_LIMIT: %w", err)
	}
	// Requests per minute allowed on /api/auth from one client address; the
	// burst is twice it. There is deliberately no value that turns the bucket
	// off -- a suite that would trip it raises the number instead.
	if cfg.AuthRatePerMin, err = parseCount(env("DRIVE_AUTH_RATE_PER_MIN", "")); err != nil {
		return nil, fmt.Errorf("DRIVE_AUTH_RATE_PER_MIN: %w", err)
	}
	// Messages the whole service may send in a day. Unlike the two limits
	// above, 0 here means no budget at all -- which is right for a local
	// Mailpit and wrong for anything with a vendor quota, so Load defaults it to
	// a real number and a deployment only ever raises or lowers it.
	//
	// The default is half a typical 100/day vendor allowance, not all of it,
	// because the budget's window is anchored to its first event rather than to
	// a calendar day: a full budget spent at the end of one window and another
	// at the start of the next can land inside a single vendor day. Half means
	// even that worst case stays under.
	if cfg.EmailDailyCap, err = parseCount(env("DRIVE_EMAIL_DAILY_CAP", "45")); err != nil {
		return nil, fmt.Errorf("DRIVE_EMAIL_DAILY_CAP: %w", err)
	}
	if cfg.PartSize, err = ParseSize(env("DRIVE_PART_SIZE", "100MiB")); err != nil {
		return nil, fmt.Errorf("DRIVE_PART_SIZE: %w", err)
	}
	// The three volume caps. All default to 0, which means no cap: the test
	// battery deliberately uploads multi-GB files, and a default that refused
	// them would be a limit nobody chose. A deployment sets real numbers -- and
	// StorageCap in particular is what bounds the object store's bill, since the
	// store itself offers no spend limit.
	if cfg.MaxFileSize, err = ParseSize(env("DRIVE_MAX_FILE_SIZE", "0")); err != nil {
		return nil, fmt.Errorf("DRIVE_MAX_FILE_SIZE: %w", err)
	}
	if cfg.StorageCap, err = ParseSize(env("DRIVE_STORAGE_CAP", "0")); err != nil {
		return nil, fmt.Errorf("DRIVE_STORAGE_CAP: %w", err)
	}
	if cfg.UserQuota, err = ParseSize(env("DRIVE_USER_QUOTA", "0")); err != nil {
		return nil, fmt.Errorf("DRIVE_USER_QUOTA: %w", err)
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
		// The S3 credentials and the signing region are required here rather
		// than discovered later. Without them the server boots perfectly well
		// and then fails at the first CreateMultipartUpload -- with a blank
		// region, with SignatureDoesNotMatch -- so the failure surfaces inside
		// somebody's upload instead of at startup, where it belongs.
		{"DRIVE_S3_ACCESS_KEY", c.S3AccessKey},
		{"DRIVE_S3_SECRET_KEY", c.S3SecretKey},
		{"DRIVE_S3_REGION", c.S3Region},
	}
	// DRIVE_MAILPIT_API is deliberately absent: nothing in the server reads it.
	// It belongs to the test harness's inbox client, which validates it itself.
	// Exactly one mail path has to be configured. Which one is decided by
	// DRIVE_RESEND_KEY, so the other's variable is not required.
	if !c.UseResend() {
		required = append(required, struct {
			name  string
			value string
		}{"DRIVE_SMTP_ADDR", c.SMTPAddr})
	}
	for _, r := range required {
		if strings.TrimSpace(r.value) == "" {
			return fmt.Errorf("%s: must not be empty", r.name)
		}
	}

	switch c.SignupMode {
	case "", SignupOpen, SignupInvite, SignupClosed:
	default:
		return fmt.Errorf("DRIVE_SIGNUP_MODE: %q is not one of open, invite, closed", c.SignupMode)
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
	// The SMTP probe applies only when SMTP is the sender. With DRIVE_RESEND_KEY
	// set, mail goes over HTTPS and nothing is listening on an SMTP port -- and
	// on Railway's Hobby plan nothing can be, since outbound SMTP is blocked
	// there entirely. Probing anyway would hard-fail boot on the one platform
	// this configuration exists for. Resend itself is deliberately not probed:
	// a third party being briefly unreachable must not stop the server starting.
	if !c.UseResend() {
		if err := dialTCP(ctx, c.SMTPAddr); err != nil {
			return fmt.Errorf("DRIVE_SMTP_ADDR: %w", err)
		}
	}
	return nil
}

// UseResend reports whether mail goes over Resend's HTTP API rather than SMTP.
// The key's presence is the whole switch: there is no mode variable to get out
// of step with it.
func (c *Config) UseResend() bool { return strings.TrimSpace(c.ResendKey) != "" }

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

// parseCount reads a non-negative integer setting. An empty value is 0, which
// every caller reads as "unset": a limit falls back to its own default and a cap
// means unlimited. Which of the two is documented at each field.
func parseCount(s string) (int, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(t)
	if err != nil {
		return 0, fmt.Errorf("invalid count %q: want a non-negative integer", s)
	}
	if n < 0 {
		return 0, fmt.Errorf("invalid count %q: must not be negative", s)
	}
	return n, nil
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
