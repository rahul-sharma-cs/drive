# Drive — build orchestration.
# Target semantics here are load-bearing, not cosmetic: the Go test harness and
# the e2e runner both depend on the exact stack names, flags and ordering below.
# Changing what a target does breaks callers that never read this file.
#
# Every Go command runs from the repo root: go.work puts the server module in a
# workspace so `go run ./server/cmd/...` resolves, and relative paths (.env,
# e2e/, server/cmd/spike/public) mean what they say.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

# Go lives only at /opt/homebrew/bin on this machine.
export PATH := /opt/homebrew/bin:$(PATH)

DEV_STACK  := docker compose -p drive --env-file .env
TEST_STACK := docker compose -p drive-test --env-file .env.test

.PHONY: doctor infra-init infra-init-test dev seed seed-test test test-big test-50g e2e \
	e2e-typecheck build token verify-public spike up down up-test down-test

## -- stack lifecycle --------------------------------------------------------

# `.env` is an order-only prerequisite: on a fresh clone it does not exist, and
# compose would then start Garage with an empty GARAGE_RPC_SECRET (it exits
# immediately). infra-init writes the file AND brings the stack up itself.
up: | .env
	$(DEV_STACK) up -d

down:
	$(DEV_STACK) down

up-test:
	$(TEST_STACK) up -d

down-test:
	$(TEST_STACK) down -v

.env:
	go run ./server/cmd/infra-init -project drive -env-file .env

## -- day-to-day --------------------------------------------------------------

doctor:
	@bash scripts/doctor.sh

infra-init:
	go run ./server/cmd/infra-init -project drive -env-file .env

# One infra-init pass against the drive-test stack.
# infra-init loads .env.test itself — do not also source it here, or a stale
# dev value could leak in.
infra-init-test:
	go run ./server/cmd/infra-init -project drive-test -env-file .env.test

spike: infra-init
	go run ./server/cmd/spike -env-file .env

dev: up
	@trap 'kill 0' EXIT INT TERM; \
	( set -a; . ./.env; set +a; go run ./server/cmd/drive ) & \
	( cd web && npm run dev ) & \
	wait

build:
	cd web && npm run build
	go build -o server/drive ./server/cmd/drive
	@if [ -d server/cmd/drive-mcp ]; then \
		go build -o server/drive-mcp ./server/cmd/drive-mcp; \
	else \
		echo "build: server/cmd/drive-mcp does not exist yet — skipping"; \
	fi

## -- tests --------------------------------------------------------------

# -p 1: every DB-backed package points at the same drive-test Postgres, and the
# seed and integration packages reset the schema. Running package tests in
# parallel tears the schema out from under the others mid-query.
test: infra-init-test
	@set -a; . ./.env.test; set +a; go test -p 1 -count=1 ./server/...

# Playwright transpiles the specs with esbuild and never type-checks them: a
# call that does not exist, or an option a version removed, compiles fine and
# fails at the line that runs it -- thirty specs into a run that had to bring a
# stack up first. This is the same check the tsconfig in e2e/ exists for, and it
# is a prerequisite of `e2e` rather than a step inside it so that it fails
# before docker and the web build are asked for anything.
e2e-typecheck:
	cd e2e && npx tsc --noEmit -p .

e2e: e2e-typecheck infra-init-test build
	$(TEST_STACK) exec -T postgres \
		psql -U drive -d drive -c 'DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;'
	$(MAKE) seed-test
	@set -a; . ./.env.test; set +a; \
	base="http://localhost$${DRIVE_ADDR}"; \
	DRIVE_PART_SIZE=10MiB ./server/drive & \
	SERVER_PID=$$!; \
	trap 'kill $$SERVER_PID 2>/dev/null || true' EXIT; \
	healthy=0; \
	for i in $$(seq 1 60); do \
		if curl -sf "$$base/healthz" >/dev/null; then healthy=1; break; fi; \
		sleep 1; \
	done; \
	if [ "$$healthy" -ne 1 ]; then echo "e2e: server did not become healthy at $$base within 60s" >&2; exit 1; fi; \
	cd e2e && E2E_BASE_URL="$$base" npx playwright test --project=chromium --workers=1 --retries=0

# The browser resume proof: it spawns, SIGKILLs and restarts the server itself
# mid-upload, so it cannot share `make e2e`'s single server and is excluded from
# that run by playwright.config.ts unless DRIVE_RESUME_SPEC is set.
e2e-resume: infra-init-test build
	cd e2e && DRIVE_RESUME_SPEC=1 npx playwright test resume.spec.ts \
		--project=chromium --workers=1 --retries=0

# Idempotent: a second run is a no-op once the users exist.
seed:
	go run ./server/cmd/seed -env-file .env

seed-test:
	go run ./server/cmd/seed -env-file .env.test

# Milestones and handoffs only, never the per-loop battery: these take tens of
# minutes and would make the routine loop unusable. The loop battery covers the
# same code paths with 100-200 MB files at 10 MiB parts.
# Multi-GB random file end to end, plus the >1,000-part ListParts pagination
# case (sparse ~11 GiB at 10 MiB parts). Fixtures land in the gitignored
# e2e/fixtures/big and are reused, so a second run does not rebuild them.
# -timeout 0: these legitimately outlive go test's 10m default.
test-big: infra-init-test
	@set -a; . ./.env.test; set +a; \
	DRIVE_TEST_BIG=1 go test -p 1 -count=1 -timeout 0 -v \
		-run 'TestBigMultiGBRoundTrip|TestBigListPartsPagination' ./server/integration/

# Opt-in 50 GB run: sparse file via /usr/bin/truncate. Manual trigger only —
# run it once when the battery is green and once before handoff.
test-50g: infra-init-test
	@set -a; . ./.env.test; set +a; \
	DRIVE_TEST_50G=1 go test -p 1 -count=1 -timeout 0 -v \
		-run 'TestBigFiftyGB' ./server/integration/

token:
	@echo "make token: drive-token does not exist yet" >&2; exit 1
# The shape this target will take, recorded now so the argument contract is
# settled before drive-token exists. USER and SCOPES carry no secret; the generated token is
# printed once by the command and never appears in argv:
# token:
#	go run ./server/cmd/drive-token -user $(USER) -scopes $(SCOPES)

## -- release hygiene ----------------------------------------------------

# Asserts the repo is safe to push to the public GitHub remote:
#   - nothing under docs/ is tracked
#   - no .env* file is tracked except the allowlist {.env.example, .env.test}
#     (.env.example holds obvious placeholders; .env.test holds only throwaway
#     test-stack constants — rebound ports and fixed test credentials that
#     exist nowhere else. Nothing else under .env* may be tracked.)
#   - docs/build, docs/research, docs/summary are all gitignored. The trailing
#     slash on the path is load-bearing: the patterns are directory-form
#     ("docs/build/"), and git check-ignore matches those only against a path
#     it can tell is a directory. It reads the working tree to decide, so in a
#     git worktree -- where docs/ does not exist at all, because it is ignored
#     -- the bare name matches nothing and this check reported a false FAIL.
#   - the go:embed placeholder is tracked, so a fresh clone compiles
#   - no AWS-key-shaped string, PEM private-key header, or drv_ PAT-shaped
#     string appears in any tracked file's current content
verify-public:
	@fail=0; \
	echo "== verify-public =="; \
	docs_tracked=$$(git ls-files docs); \
	if [ -n "$$docs_tracked" ]; then \
		echo "FAIL: files tracked under docs/:"; echo "$$docs_tracked"; fail=1; \
	else \
		echo "PASS: no docs/ files tracked"; \
	fi; \
	env_files=$$(git ls-files | grep -E '(^|/)\.env[^/]*$$' || true); \
	if [ -n "$$env_files" ]; then \
		bad_env=$$(printf '%s\n' "$$env_files" | grep -vE '(^|/)\.env\.(example|test)$$' || true); \
		if [ -n "$$bad_env" ]; then \
			echo "FAIL: unexpected .env* file(s) tracked:"; echo "$$bad_env"; fail=1; \
		else \
			echo "PASS: only allowlisted .env files tracked:"; echo "$$env_files"; \
		fi; \
	else \
		echo "PASS: no .env* files tracked"; \
	fi; \
	for d in docs/build docs/research docs/summary; do \
		if git check-ignore -q "$$d/"; then \
			echo "PASS: $$d/ is gitignored"; \
		else \
			echo "FAIL: $$d/ is NOT gitignored"; fail=1; \
		fi; \
	done; \
	if git ls-files --error-unmatch server/web/dist/index.html >/dev/null 2>&1; then \
		echo "PASS: server/web/dist/index.html is tracked (go:embed compiles on a fresh clone)"; \
	else \
		echo "FAIL: server/web/dist/index.html is not tracked — go:embed cannot compile on a fresh clone"; fail=1; \
	fi; \
	secret_hits=$$(git grep -InE '(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}|BEGIN[A-Z ]*PRIVATE KEY|drv_[0-9A-Za-z]{30,}' -- . 2>/dev/null || true); \
	if [ -n "$$secret_hits" ]; then \
		echo "FAIL: possible secret(s) found in tracked files:"; echo "$$secret_hits"; fail=1; \
	else \
		echo "PASS: no secret-shaped strings found in tracked files"; \
	fi; \
	leaked=$$(for f in .env .env.r2 .env.prod .env.prod.account; do \
		[ -f "$$f" ] || continue; \
		sed -n 's/^[A-Za-z_][A-Za-z0-9_]*=//p' "$$f" \
		| sed 's/^["'"'"']//; s/["'"'"']$$//' \
		| grep -E '^.{12,}$$' \
		| grep -v 'generated-by-make-infra-init' \
		| grep -vE 'localhost|127\.0\.0\.1' \
		| while IFS= read -r v; do \
			git grep -lF -- "$$v" 2>/dev/null | sed "s|^|  a value from $$f appears in |"; \
		done; \
	done); \
	if [ -n "$$leaked" ]; then \
		echo "FAIL: an untracked env file's value is present in a tracked file:"; echo "$$leaked"; fail=1; \
	else \
		echo "PASS: no untracked env-file values appear in tracked files"; \
	fi; \
	if [ "$$fail" -ne 0 ]; then \
		echo "verify-public: FAILED"; \
		exit 1; \
	fi; \
	echo "verify-public: all checks passed"
