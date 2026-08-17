// Command infra-init prepares the local object store: it generates the .env
// secrets on first run, waits for Garage, provisions the key and bucket if the
// container's env auto-provisioning did not, and applies the bucket CORS rules
// the browser upload path depends on. Running it twice is a no-op.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/rahul-sharma-cs/drive/server/internal/blob"
	"github.com/rahul-sharma-cs/drive/server/internal/config"
)

const (
	// viteOrigin is the dev server; the other allowed origin comes from
	// DRIVE_BASE_URL, so the test stack (:8081) is picked up automatically.
	viteOrigin = "http://localhost:5173"
	garageWait = 60 * time.Second
	// garageKeyName labels the imported key in Garage's key list.
	garageKeyName = "drive"
)

func main() {
	project := flag.String("project", "drive", "docker compose project name (Garage CLI fallback)")
	envFile := flag.String("env-file", ".env", "env file to create/complete and load")
	flag.Parse()

	l := log.New(os.Stdout, "", 0)
	if err := run(context.Background(), l, *project, *envFile); err != nil {
		l.Printf("infra-init failed: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, l *log.Logger, project, envPath string) error {
	vals, err := ensureEnvFile(l, envPath)
	if err != nil {
		return err
	}
	// The env file is authoritative — the caller need not have sourced it, and
	// `-env-file .env.test` must win over a dev shell.
	for k, v := range vals {
		if err := os.Setenv(k, v); err != nil {
			return fmt.Errorf("set %s: %w", k, err)
		}
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		return fmt.Errorf("DRIVE_S3_ACCESS_KEY/DRIVE_S3_SECRET_KEY are empty in %s", envPath)
	}
	if err := guardLocalEndpoint(cfg.S3Endpoint); err != nil {
		return err
	}

	// Bring the stack up with the values we just resolved. This has to happen
	// here, not in a Makefile prerequisite: on a first run the env file does not
	// exist yet, so a `docker compose up` before this point starts Garage with an
	// empty GARAGE_RPC_SECRET and it exits immediately ("Invalid RPC secret key")
	// — waitForGarage would then poll a dead container for its whole deadline.
	// Compose only recreates containers whose configuration actually changed, so
	// running this every time is a no-op once the stack is healthy.
	if err := composeUp(ctx, l, project, envPath); err != nil {
		return err
	}

	client, _, err := blob.New(ctx, cfg)
	if err != nil {
		return err
	}

	ready, err := waitForGarage(ctx, l, client, cfg)
	if err != nil {
		return err
	}
	if ready {
		l.Printf("bucket %q already provisioned — no Garage CLI fallback needed", cfg.S3Bucket)
	} else {
		if err := provision(ctx, l, project, envPath, cfg); err != nil {
			return err
		}
		if ready, err = waitForGarage(ctx, l, client, cfg); err != nil {
			return err
		}
		if !ready {
			return fmt.Errorf("bucket %q still not reachable with the configured key after provisioning", cfg.S3Bucket)
		}
	}

	origins, err := corsOrigins(cfg.BaseURL)
	if err != nil {
		return err
	}
	if err := putCORS(ctx, client, cfg.S3Bucket, origins); err != nil {
		return err
	}

	l.Printf("")
	l.Printf("ready:")
	l.Printf("  endpoint : %s", cfg.S3Endpoint)
	l.Printf("  bucket   : %s", cfg.S3Bucket)
	l.Printf("  cors     : %s", strings.Join(origins, ", "))
	l.Printf("  env file : %s", envPath)
	return nil
}

// ---------------------------------------------------------------- env file

// secretGroups map generated values onto the keys that must carry them. The
// server and the Garage container read the same credential under two names.
var secretGroups = []struct {
	keys []string
	gen  func() (string, error)
}{
	{[]string{"GARAGE_RPC_SECRET"}, hex32},
	{[]string{"GARAGE_DEFAULT_ACCESS_KEY", "DRIVE_S3_ACCESS_KEY"}, accessKey},
	{[]string{"GARAGE_DEFAULT_SECRET_KEY", "DRIVE_S3_SECRET_KEY"}, hex32},
}

// ensureEnvFile creates the env file from .env.example when missing and fills
// in blank or placeholder secrets. Existing real values are never rotated.
func ensureEnvFile(l *log.Logger, path string) (map[string]string, error) {
	examplePath := filepath.Join(filepath.Dir(path), ".env.example")
	example, exampleErr := readEnvFile(examplePath)

	f, err := readEnvFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if exampleErr != nil {
			return nil, fmt.Errorf("%s is missing and %s is unreadable: %w", path, examplePath, exampleErr)
		}
		l.Printf("creating %s from %s", path, examplePath)
		f = &envFile{lines: append([]string(nil), example.lines...)}
	case err != nil:
		return nil, err
	default:
		l.Printf("using existing %s", path)
	}

	var generated, filled []string
	for _, g := range secretGroups {
		value := ""
		for _, k := range g.keys {
			if v := f.get(k); !isPlaceholder(v, exampleValue(example, k)) {
				value = v
				break
			}
		}
		if value == "" {
			if value, err = g.gen(); err != nil {
				return nil, err
			}
			generated = append(generated, g.keys...)
		}
		for _, k := range g.keys {
			if isPlaceholder(f.get(k), exampleValue(example, k)) {
				f.set(k, value)
				filled = append(filled, k)
			}
		}
	}

	if len(filled) > 0 {
		// Names only — a secret must never reach stdout.
		l.Printf("writing %s (0600): %s", path, strings.Join(filled, ", "))
		if len(generated) > 0 {
			l.Printf("  newly generated: %s", strings.Join(generated, ", "))
		}
		if err := os.WriteFile(path, []byte(f.render()), 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", path, err)
		}
	} else {
		l.Printf("%s already carries every secret — nothing rotated", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return f.values(), nil
}

// envFile keeps the file's lines so rewrites preserve comments and ordering.
type envFile struct {
	lines []string
}

func readEnvFile(path string) (*envFile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(b), "\n")
	return &envFile{lines: strings.Split(text, "\n")}, nil
}

func splitLine(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false
	}
	k, v, found := strings.Cut(t, "=")
	if !found {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		v = v[1 : len(v)-1]
	}
	return strings.TrimSpace(k), v, true
}

func (f *envFile) get(key string) string {
	for _, line := range f.lines {
		if k, v, ok := splitLine(line); ok && k == key {
			return v
		}
	}
	return ""
}

func (f *envFile) set(key, value string) {
	for i, line := range f.lines {
		if k, _, ok := splitLine(line); ok && k == key {
			f.lines[i] = key + "=" + value
			return
		}
	}
	f.lines = append(f.lines, key+"="+value)
}

func (f *envFile) values() map[string]string {
	out := map[string]string{}
	for _, line := range f.lines {
		if k, v, ok := splitLine(line); ok {
			out[k] = v
		}
	}
	return out
}

func (f *envFile) render() string { return strings.Join(f.lines, "\n") + "\n" }

func exampleValue(example *envFile, key string) string {
	if example == nil {
		return ""
	}
	return example.get(key)
}

// isPlaceholder reports whether a value still needs to be filled in. A value
// identical to .env.example's is the reliable signal; the literal patterns
// cover hand-edited files.
func isPlaceholder(value, example string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return true
	}
	if example != "" && v == strings.TrimSpace(example) {
		return true
	}
	if strings.HasPrefix(v, "<") && strings.HasSuffix(v, ">") {
		return true
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"generated-by", "changeme", "change-me", "replace-me", "placeholder"} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// hex32 returns 64 hex characters — the shape Garage expects for
// GARAGE_RPC_SECRET and a comfortable S3 secret key.
func hex32() (string, error) { return randomHex(32) }

// accessKey returns a Garage-style access key id: "GK" + 30 hex characters.
func accessKey() (string, error) {
	s, err := randomHex(15)
	if err != nil {
		return "", err
	}
	return "GK" + s, nil
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// ---------------------------------------------------------------- garage

// waitForGarage polls until Garage answers, and reports whether the bucket is
// already usable with the configured key. A Garage-issued API error (bad key,
// no permission) means Garage is up but unprovisioned — stop waiting and let
// the CLI fallback fix it.
func waitForGarage(ctx context.Context, l *log.Logger, c *s3.Client, cfg *config.Config) (bool, error) {
	deadline := time.Now().Add(garageWait)
	for attempt := 0; ; attempt++ {
		out, err := c.ListBuckets(ctx, &s3.ListBucketsInput{})
		if err == nil {
			for _, b := range out.Buckets {
				if aws.ToString(b.Name) == cfg.S3Bucket {
					return true, nil
				}
			}
			l.Printf("garage is up; bucket %q is not visible to this key", cfg.S3Bucket)
			return false, nil
		}

		var apiErr smithy.APIError
		if errors.As(err, &apiErr) {
			l.Printf("garage is up but refused the request (%s); provisioning the key", apiErr.ErrorCode())
			return false, nil
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("garage at %s unreachable after %s: %w", cfg.S3Endpoint, garageWait, err)
		}
		l.Printf("waiting for garage at %s ... (attempt %d, %s left)",
			cfg.S3Endpoint, attempt+1, time.Until(deadline).Round(time.Second))
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// composeUp starts (or recreates) the compose stack this env file describes.
func composeUp(ctx context.Context, l *log.Logger, project, envPath string) error {
	l.Printf("docker compose -p %s --env-file %s up -d", project, envPath)
	out, err := exec.CommandContext(ctx, "docker", "compose",
		"-p", project, "--env-file", envPath, "up", "-d").CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker compose up: %w\n%s", err, out)
	}
	return nil
}

// provision runs the Garage CLI through docker for the case where the
// container's --default-bucket auto-provisioning did not take effect.
func provision(ctx context.Context, l *log.Logger, project, envPath string, cfg *config.Config) error {
	l.Printf("provisioning key and bucket through the Garage CLI")
	steps := [][]string{
		{"key", "import", "--yes", "-n", garageKeyName, cfg.S3AccessKey, cfg.S3SecretKey},
		{"bucket", "create", cfg.S3Bucket},
		{"bucket", "allow", "--read", "--write", "--owner", cfg.S3Bucket, "--key", cfg.S3AccessKey},
	}
	for _, step := range steps {
		out, err := garageCLI(ctx, l, project, envPath, cfg.S3SecretKey, step)
		if err != nil {
			if strings.Contains(strings.ToLower(out), "already exist") {
				l.Printf("    already present, continuing")
				continue
			}
			return fmt.Errorf("garage %s: %w\n%s", strings.Join(step, " "), err, out)
		}
	}
	return nil
}

func garageCLI(ctx context.Context, l *log.Logger, project, envPath, secret string, args []string) (string, error) {
	full := append([]string{
		"compose", "-p", project, "--env-file", envPath, "exec", "-T", "garage", "/garage",
	}, args...)

	printed := make([]string, len(full))
	for i, a := range full {
		if secret != "" && a == secret {
			a = "****"
		}
		printed[i] = a
	}
	l.Printf("    docker %s", strings.Join(printed, " "))

	out, err := exec.CommandContext(ctx, "docker", full...).CombinedOutput()
	return string(out), err
}

// ---------------------------------------------------------------- cors

func corsOrigins(baseURL string) ([]string, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("DRIVE_BASE_URL %q is not an absolute URL", baseURL)
	}
	origins := []string{viteOrigin}
	if app := u.Scheme + "://" + u.Host; app != viteOrigin {
		origins = append(origins, app)
	}
	return origins, nil
}

// putCORS writes one rule per origin. Garage joins the origins of a single rule
// into one Access-Control-Allow-Origin header, which browsers reject when it
// lists two — do not merge these rules.
func putCORS(ctx context.Context, c *s3.Client, bucket string, origins []string) error {
	rules := make([]types.CORSRule, 0, len(origins))
	for _, o := range origins {
		rules = append(rules, types.CORSRule{
			AllowedOrigins: []string{o},
			AllowedMethods: []string{"PUT", "GET"},
			// Without "*", a part PUT carrying a Content-Type fails preflight.
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
		})
	}
	_, err := c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket:            aws.String(bucket),
		CORSConfiguration: &types.CORSConfiguration{CORSRules: rules},
	})
	if err != nil {
		return fmt.Errorf("put bucket cors: %w", err)
	}
	return nil
}

// guardLocalEndpoint refuses to bootstrap anything but a local object store.
//
// This command runs `docker compose up`, creates a bucket and writes a CORS
// rule naming a localhost dev origin. Pointed at a hosted store it would either
// fail confusingly or, worse, succeed -- replacing a production CORS rule with
// one that trusts http://localhost. There is no flag to override it: nothing
// about this command has a use against a remote store.
func guardLocalEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("DRIVE_S3_ENDPOINT: not a valid URL: %w", err)
	}
	switch u.Hostname() {
	case "localhost", "127.0.0.1", "::1", "garage", "":
		return nil
	}
	return fmt.Errorf(
		"refusing to bootstrap %s: infra-init brings up the local docker stack and writes a bucket CORS rule "+
			"naming a localhost origin, which would overwrite a hosted store's own rule. It only ever runs "+
			"against the local Garage", u.Hostname())
}
