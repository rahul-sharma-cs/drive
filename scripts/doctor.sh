#!/usr/bin/env bash
# drive doctor — preflight checks before bringing up the stack.
# Every check prints PASS/FAIL/WARN/INFO and never aborts early: we want the
# full report in one run. Exits non-zero iff any check FAILed.
set -u -o pipefail

# Go lives only at /opt/homebrew/bin on this machine.
export PATH="/opt/homebrew/bin:$PATH"

fail=0
pass() { printf 'PASS: %s\n' "$1"; }
fail_() { printf 'FAIL: %s\n' "$1"; fail=1; }
warn() { printf 'WARN: %s\n' "$1"; }
info() { printf 'INFO: %s\n' "$1"; }

echo "== drive doctor =="

docker_up=0
if docker info >/dev/null 2>&1; then
	pass "Docker daemon is running"
	docker_up=1
else
	fail_ "Docker daemon is not running — start Docker Desktop"
fi

# -- compose ports: free, or owned by one of our compose stacks -------------
# Docker Desktop binds published ports through its own backend process, so a
# plain `lsof` cannot attribute a listener to a container — ask docker which
# compose project (if any) published the port instead.
check_port() {
	port="$1"
	label="$2"
	listener=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null || true)
	if [ -z "$listener" ]; then
		pass "port $port ($label) is free"
		return
	fi
	if [ "$docker_up" -ne 1 ]; then
		fail_ "port $port ($label) is occupied and docker is unreachable to check ownership"
		return
	fi
	project=$(docker ps --filter "publish=$port" --format '{{.Label "com.docker.compose.project"}}' 2>/dev/null | head -n1)
	case "$project" in
	drive | drive-test)
		pass "port $port ($label) is in use by our compose stack ($project)"
		;;
	*)
		fail_ "port $port ($label) is occupied by something other than our compose stack"
		;;
	esac
}

echo "-- ports (dev stack) --"
check_port 3900 "garage s3"
check_port 55432 "postgres"
check_port 1025 "mailpit smtp"
check_port 8025 "mailpit ui/api"

echo "-- ports (test stack) --"
check_port 3910 "garage s3"
check_port 55433 "postgres"
check_port 1026 "mailpit smtp"
check_port 8026 "mailpit ui/api"

# -- host disk space ---------------------------------------------------------
echo "-- disk space --"
host_avail_kb=$(df -k / | awk 'NR==2 {print $4}')
host_avail_gb=$((host_avail_kb / 1024 / 1024))
if [ "$host_avail_gb" -lt 20 ]; then
	warn "host free disk: ${host_avail_gb} GB (< 20 GB — make test-big/test-50g may fail)"
else
	pass "host free disk: ${host_avail_gb} GB"
fi

# -- Docker VM free disk + host<->VM clock skew ------------------------------
# One throwaway container gives us both numbers from the same run.
if [ "$docker_up" -eq 1 ]; then
	host_before=$(date -u +%s)
	vm_out=$(docker run --rm alpine sh -c 'date -u +%s; df -k / | awk "NR==2{print \$4}"' 2>/dev/null || true)
	host_after=$(date -u +%s)
	if [ -n "$vm_out" ]; then
		vm_time=$(printf '%s\n' "$vm_out" | sed -n '1p')
		vm_avail_kb=$(printf '%s\n' "$vm_out" | sed -n '2p')
		host_mid=$(((host_before + host_after) / 2))
		skew=$((host_mid - vm_time))
		skew_abs=${skew#-}
		if [ "$skew_abs" -gt 60 ]; then
			fail_ "host<->Docker-VM clock skew is ${skew}s (>60s) — restart Docker Desktop"
		else
			pass "host<->Docker-VM clock skew is ${skew}s"
		fi
		vm_avail_gb=$((vm_avail_kb / 1024 / 1024))
		if [ "$vm_avail_gb" -lt 20 ]; then
			warn "Docker VM free disk: ${vm_avail_gb} GB (< 20 GB — make test-big/test-50g may fail)"
		else
			pass "Docker VM free disk: ${vm_avail_gb} GB"
		fi
	else
		warn "could not run a throwaway container — clock skew and VM disk unchecked (offline image pull?)"
	fi
else
	warn "docker daemon unreachable — clock skew and VM disk unchecked"
fi

# -- Docker Desktop disk-image size limit (informational only) --------------
# `make test-big`/`test-50g` write an incompressible multi-GB file inside the
# VM's disk image; report the configured cap if we can find it. Location and
# key name vary by Docker Desktop version, so this is best-effort and never
# fails the check.
echo "-- docker desktop disk-image limit (informational) --"
disksize=""
for settings_path in \
	"$HOME/Library/Group Containers/group.com.docker/settings-store.json" \
	"$HOME/Library/Group Containers/group.com.docker/settings.json" \
	"$HOME/Library/Application Support/Docker Desktop/settings-store.json" \
	"$HOME/Library/Application Support/Docker Desktop/settings.json"; do
	if [ -f "$settings_path" ]; then
		match=$(grep -io '"disksize[a-z]*"[[:space:]]*:[[:space:]]*[0-9]*' "$settings_path" 2>/dev/null | head -n1 || true)
		if [ -n "$match" ]; then
			disksize="$match (from $settings_path)"
			break
		fi
	fi
done
if [ -n "$disksize" ]; then
	info "Docker Desktop disk-image size limit setting: $disksize"
else
	info "Docker Desktop disk-image size limit: unknown (settings file not found or key not present)"
fi

echo "===================="
if [ "$fail" -ne 0 ]; then
	echo "doctor: FAIL — fix the above before continuing"
	exit 1
fi
echo "doctor: PASS"
