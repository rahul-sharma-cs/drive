package config

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseSize(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "100MiB", want: 100 * MiB},
		{in: "10MiB", want: 10 * MiB},
		{in: "10mib", want: 10 * MiB},
		{in: " 10 MiB ", want: 10 * MiB},
		{in: "104857600", want: 100 * MiB},
		{in: "512KiB", want: 512 * KiB},
		{in: "2GiB", want: 2 * GiB},
		// Decimal suffixes: what a quota is quoted in. "MB" must not be
		// mistaken for the bare "B" case, which would leave "10M" to parse.
		{in: "10MB", want: 10 * MB},
		{in: "3GB", want: 3 * GB},
		{in: "512kb", want: 512 * KB},
		{in: "2GB", want: 2_000_000_000},
		{in: "0", want: 0},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
		{in: "-5MiB", wantErr: true},
	}
	for _, c := range cases {
		got, err := ParseSize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseSize(%q) = %d, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSize(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseSize(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestValidatePartSize(t *testing.T) {
	cases := []struct {
		size int64
		ok   bool
	}{
		{size: 10 * MiB, ok: true},
		{size: 100 * MiB, ok: true},
		{size: 5 * MiB, ok: false},  // meets the S3 floor, not a 10MiB multiple
		{size: 15 * MiB, ok: false}, // not a 10MiB multiple
		{size: 1 * MiB, ok: false},  // below the S3 floor
		{size: 0, ok: false},
	}
	for _, c := range cases {
		cfg := validConfig()
		cfg.PartSize = c.size
		err := cfg.Validate()
		if c.ok && err != nil {
			t.Errorf("Validate() with part size %d: unexpected error: %v", c.size, err)
		}
		if !c.ok && err == nil {
			t.Errorf("Validate() with part size %d: want error, got nil", c.size)
		}
	}
}

func TestValidateRequiredValues(t *testing.T) {
	blank := func(c *Config) *Config { return c }
	cases := []struct {
		name string
		mut  func(*Config) *Config
	}{
		{"DRIVE_ADDR", func(c *Config) *Config { c.Addr = ""; return c }},
		{"DRIVE_BASE_URL", func(c *Config) *Config { c.BaseURL = ""; return c }},
		{"DRIVE_DB_DSN", func(c *Config) *Config { c.DBDSN = ""; return c }},
		{"DRIVE_S3_ENDPOINT", func(c *Config) *Config { c.S3Endpoint = ""; return c }},
		{"DRIVE_S3_BUCKET", func(c *Config) *Config { c.S3Bucket = ""; return c }},
		// Credentials and the signing region are required at boot, not
		// discovered at the first upload: a blank region boots fine and then
		// fails every CreateMultipartUpload with SignatureDoesNotMatch.
		{"DRIVE_S3_ACCESS_KEY", func(c *Config) *Config { c.S3AccessKey = ""; return c }},
		{"DRIVE_S3_SECRET_KEY", func(c *Config) *Config { c.S3SecretKey = ""; return c }},
		{"DRIVE_S3_REGION", func(c *Config) *Config { c.S3Region = ""; return c }},
	}
	for _, c := range cases {
		if err := c.mut(validConfig()).Validate(); err == nil {
			t.Errorf("Validate() with empty %s: want error, got nil", c.name)
		}
	}
	if err := blank(validConfig()).Validate(); err != nil {
		t.Errorf("Validate() on a valid config: unexpected error: %v", err)
	}
}

func TestLoadDefaults(t *testing.T) {
	// An empty value falls back to the default, so this isolates the test from
	// whatever the developer's shell exports -- including the credentials a
	// sourced .env leaves behind, which would make the assertion below vacuous.
	for _, k := range []string{"DRIVE_ADDR", "DRIVE_BASE_URL", "DRIVE_DB_DSN", "DRIVE_S3_ENDPOINT",
		"DRIVE_S3_BUCKET", "DRIVE_S3_ACCESS_KEY", "DRIVE_S3_SECRET_KEY", "DRIVE_S3_REGION",
		"DRIVE_SMTP_ADDR", "DRIVE_MAILPIT_API", "DRIVE_PART_SIZE", "DRIVE_RESEND_KEY",
		"DRIVE_MAIL_FROM", "DRIVE_SIGNUP_MODE", "DRIVE_LOG_LEVEL",
		"DRIVE_PRESIGN_TTL", "DRIVE_TOKEN_PRESIGN_TTL"} {
		t.Setenv(k, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.PartSize != 100*MiB {
		t.Errorf("default part size = %d, want %d", cfg.PartSize, 100*MiB)
	}
	// The signing region has no default on purpose: one would make the
	// Validate requirement below unreachable, and a wrong region does not fail
	// at boot -- it fails at the first CreateMultipartUpload, inside a user's
	// upload. blob.New keeps its own fallback for hand-built Config literals.
	if cfg.S3Region != "" {
		t.Errorf("signing region = %q, want empty when the variable is unset", cfg.S3Region)
	}
	// An environment that sets nothing is a deployment, not a laptop: it gets
	// the quiet log level, and the dev and test .env files ask for debug.
	if cfg.LogLevel != "info" {
		t.Errorf("default log level = %q, want info", cfg.LogLevel)
	}
	if cfg.SignupMode != SignupOpen {
		t.Errorf("default signup mode = %q, want %q", cfg.SignupMode, SignupOpen)
	}
	// And the defaults alone are NOT a runnable configuration: there is no
	// sensible default for a credential, so Validate has to say so at boot
	// rather than let the first upload discover it.
	err = cfg.Validate()
	if err == nil {
		t.Fatal("a configuration with no S3 credentials validated")
	}
	if !strings.Contains(err.Error(), "DRIVE_S3_ACCESS_KEY") {
		t.Errorf("error = %q, want it to name the missing credential", err)
	}
}

func TestLoadPartSizeFromEnv(t *testing.T) {
	t.Setenv("DRIVE_PART_SIZE", "10MiB")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.PartSize != 10*MiB {
		t.Errorf("part size = %d, want %d", cfg.PartSize, 10*MiB)
	}

	t.Setenv("DRIVE_PART_SIZE", "nonsense")
	if _, err := Load(); err == nil {
		t.Error("Load() with a bad DRIVE_PART_SIZE: want error, got nil")
	}
}

func validConfig() *Config {
	return &Config{
		Addr:        ":8080",
		BaseURL:     "http://localhost:8080",
		DBDSN:       "postgres://drive:drive@localhost:55432/drive?sslmode=disable",
		S3Endpoint:  "http://localhost:3900",
		S3Bucket:    "drive-blobs",
		S3AccessKey: "key",
		S3SecretKey: "secret",
		S3Region:    DefaultS3Region,
		SMTPAddr:    "localhost:1025",
		MailpitAPI:  "http://localhost:8025",
		PartSize:    100 * MiB,
	}
}

// ---------------------------------------------------------------- mail path --

// DRIVE_RESEND_KEY is the only switch between the two senders, and it moves two
// things at once: which variable is required, and whether boot probes SMTP.
func TestResendKeySelectsTheMailPath(t *testing.T) {
	smtp := validConfig()
	if smtp.UseResend() {
		t.Error("a config with no DRIVE_RESEND_KEY wants the Resend path")
	}
	smtp.SMTPAddr = ""
	if err := smtp.Validate(); err == nil {
		t.Error("Validate() with no key and no DRIVE_SMTP_ADDR: want an error, got nil")
	}

	resend := validConfig()
	resend.ResendKey = "re_live_key"
	resend.SMTPAddr = ""
	if !resend.UseResend() {
		t.Error("a config with DRIVE_RESEND_KEY set does not want the Resend path")
	}
	if err := resend.Validate(); err != nil {
		t.Errorf("Validate() with a key and no DRIVE_SMTP_ADDR: %v", err)
	}
	if blank := (&Config{ResendKey: "   "}); blank.UseResend() {
		t.Error("a whitespace-only key counts as configured")
	}
}

// The probe that would hard-fail boot on Railway, where outbound SMTP is
// blocked on the Hobby plan and nothing can be listening on 1025.
//
// Everything but SMTP is stood up locally so the SMTP dial is the only thing
// that can fail: with no key it must fail and name its variable, and with a key
// it must not be attempted at all.
func TestValidateRuntimeProbesSMTPOnlyWhenSMTPIsTheSender(t *testing.T) {
	ctx := context.Background()

	db, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("standing in for Postgres: %v", err)
	}
	defer db.Close()
	go func() {
		for {
			conn, err := db.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	store := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer store.Close()

	// Port 1 on the loopback: reserved, and nothing will ever be listening.
	cfg := validConfig()
	cfg.DBDSN = "postgres://drive:drive@" + db.Addr().String() + "/drive?sslmode=disable"
	cfg.S3Endpoint = store.URL
	cfg.SMTPAddr = "127.0.0.1:1"

	err = cfg.ValidateRuntime(ctx)
	if err == nil {
		t.Fatal("ValidateRuntime passed with an unreachable SMTP server and no Resend key")
	}
	if !strings.Contains(err.Error(), "DRIVE_SMTP_ADDR") {
		t.Errorf("error = %q, want it to name DRIVE_SMTP_ADDR", err)
	}

	cfg.ResendKey = "re_live_key"
	if err := cfg.ValidateRuntime(ctx); err != nil {
		t.Errorf("ValidateRuntime probed SMTP even though mail goes over Resend: %v", err)
	}
}

// The signing-region requirement has to be reachable from a real Load(), not
// only from a hand-built Config: a default in Load() would make it unreachable
// in production while leaving the literal-fed table test green.
func TestUnsetSigningRegionFailsValidationFromLoad(t *testing.T) {
	t.Setenv("DRIVE_S3_ACCESS_KEY", "key")
	t.Setenv("DRIVE_S3_SECRET_KEY", "secret")
	t.Setenv("DRIVE_S3_REGION", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("a configuration loaded with no DRIVE_S3_REGION validated")
	}
	if !strings.Contains(err.Error(), "DRIVE_S3_REGION") {
		t.Errorf("error = %q, want it to name DRIVE_S3_REGION", err)
	}

	t.Setenv("DRIVE_S3_REGION", "auto")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.S3Region != "auto" {
		t.Errorf("signing region = %q, want auto", cfg.S3Region)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate() with a region set: %v", err)
	}
}
