# Drive — build orchestration.
# Semantics are frozen by docs/build/PLAN.md §Fixed choices "Makefile targets"
# (gitignored, not part of the public repo) — don't diverge without updating
# that doc first.
#
# Every Go command runs from the repo root: go.work puts the server module in a
# workspace so `go run ./server/cmd/...` resolves exactly as PLAN writes it, and
# relative paths (.env, e2e/, server/cmd/spike/public) mean what they say.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

# Go lives only at /opt/homebrew/bin on this machine.
export PATH := /opt/homebrew/bin:$(PATH)

DEV_STACK  := docker compose -p drive --env-file .env
TEST_STACK := docker compose -p drive-test --env-file .env.test

.PHONY: doctor infra-init infra-init-test dev seed seed-test test test-big test-50g e2e \
	build token verify-public spike up down up-test down-test

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

# One infra-init pass against the drive-test stack (PLAN §Makefile targets).
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
		echo "build: server/cmd/drive-mcp not found yet (Phase 6) — skipping"; \
	fi

## -- tests --------------------------------------------------------------

# -p 1: every DB-backed package points at the same drive-test Postgres, and the
# seed and integration packages reset the schema. Running package tests in
# parallel tears the schema out from under the others mid-query.
test: infra-init-test
	@set -a; . ./.env.test; set +a; go test -p 1 -count=1 ./server/...

e2e: infra-init-test build
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

# Idempotent: a second run is a no-op once the users exist.
seed:
	go run ./server/cmd/seed -env-file .env

seed-test:
	go run ./server/cmd/seed -env-file .env.test

test-big:
	@echo "make test-big: not implemented until Phase 2" >&2; exit 1

test-50g:
	@echo "make test-50g: not implemented until Phase 2" >&2; exit 1

token:
	@echo "make token: not implemented until Phase 6" >&2; exit 1
# Phase 6 shape (argument contract recorded now per PLAN §Fixed choices, so
# it's on record before drive-token exists):
# token:
#	go run ./server/cmd/drive-token -user $(USER) -scopes $(SCOPES)

## -- release hygiene ----------------------------------------------------

# Asserts the repo is safe to push to the public GitHub remote:
#   - nothing under docs/ is tracked
#   - no .env* file is tracked except the allowlist {.env.example, .env.test}
#     (PLAN's Makefile-targets bullet says "except .env.example"; its
#     Fixed-choices bullet says ".env.test IS committed" as throwaway test
#     constants — this allowlist is the resolution: both are permitted,
#     nothing else under .env* is)
#   - docs/build, docs/research, docs/summary are all gitignored
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
		if git check-ignore -q "$$d"; then \
			echo "PASS: $$d is gitignored"; \
		else \
			echo "FAIL: $$d is NOT gitignored"; fail=1; \
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
	if [ "$$fail" -ne 0 ]; then \
		echo "verify-public: FAILED"; \
		exit 1; \
	fi; \
	echo "verify-public: all checks passed"
