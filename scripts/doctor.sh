#!/usr/bin/env bash
# drive doctor — preflight checks before bringing up the stack.
# Every check prints PASS/FAIL/WARN/INFO and never aborts early: we want the
# full report in one run. Exits non-zero iff any check FAILed.
#
# Every probe that talks to Docker, the network or the filesystem runs under a
# timeout, and every section says what it is about to do before it does it.
# That is not tidiness: on 2026-08-22 this script sat silent for three minutes
# and then exited 0. A doctor whose own hang is indistinguishable from a slow
# machine is worse than no doctor, because it is the thing you run when you
# already suspect the environment.
set -u -o pipefail

# Go lives only at /opt/homebrew/bin on this machine.
export PATH="/opt/homebrew/bin:$PATH"

started=$(date +%s)

fail=0
pass() { printf 'PASS: %s\n' "$1"; }
fail_() { printf 'FAIL: %s\n' "$1"; fail=1; }
warn() { printf 'WARN: %s\n' "$1"; }
info() { printf 'INFO: %s\n' "$1"; }
step() { printf '   … %s\n' "$1"; }

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

# with_timeout SECONDS COMMAND... runs COMMAND and kills it after SECONDS,
# returning 124 if it had to. macOS ships no timeout(1) and this repo does not
# depend on coreutils, so the watchdog is written out by hand: the command runs
# in the background, a subshell kills it on the deadline, and whichever finishes
# first decides the answer. Command substitution around a call still captures
# stdout, because the watchdog's own output goes to /dev/null and it therefore
# never holds the pipe open.
with_timeout() {
	secs="$1"
	shift
	flag="$work_dir/timedout.$$.$RANDOM"

	"$@" &
	pid=$!
	(
		sleep "$secs"
		if kill -TERM "$pid" 2>/dev/null; then
			: >"$flag"
			sleep 2
			kill -KILL "$pid" 2>/dev/null
		fi
	) >/dev/null 2>&1 &
	watchdog=$!

	rc=0
	wait "$pid" 2>/dev/null || rc=$?
	kill -TERM "$watchdog" 2>/dev/null
	wait "$watchdog" 2>/dev/null || true

	if [ -f "$flag" ]; then
		rm -f "$flag"
		return 124
	fi
	return "$rc"
}

echo "== drive doctor =="

echo "-- docker daemon --"
step "asking the docker daemon for its info (15s limit)"
docker_up=0
with_timeout 15 docker info >/dev/null 2>&1
case $? in
0)
	pass "Docker daemon is running"
	docker_up=1
	;;
124)
	# The old script had no bound here at all, and a Docker Desktop that is
	# starting up (or wedged) answers `docker info` by not answering.
	fail_ "Docker daemon did not answer within 15s — Docker Desktop is starting up or wedged"
	;;
*)
	fail_ "Docker daemon is not running — start Docker Desktop"
	;;
esac

# -- compose ports: free, or owned by one of our compose stacks -------------
# Docker Desktop binds published ports through its own backend process, so a
# plain `lsof` cannot attribute a listener to a container — ask docker which
# compose project (if any) published the port instead.
check_port() {
	port="$1"
	label="$2"
	listener=$(with_timeout 5 lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null)
	if [ $? -eq 124 ]; then
		warn "port $port ($label): lsof did not answer within 5s — ownership unchecked"
		return
	fi
	if [ -z "$listener" ]; then
		pass "port $port ($label) is free"
		return
	fi
	if [ "$docker_up" -ne 1 ]; then
		fail_ "port $port ($label) is occupied and docker is unreachable to check ownership"
		return
	fi
	project=$(with_timeout 10 docker ps --filter "publish=$port" --format '{{.Label "com.docker.compose.project"}}' 2>/dev/null | head -n1)
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
step "probing 3900, 55432, 1025, 8025"
check_port 3900 "garage s3"
check_port 55432 "postgres"
check_port 1025 "mailpit smtp"
check_port 8025 "mailpit ui/api"

echo "-- ports (test stack) --"
step "probing 3910, 55433, 1026, 8026"
check_port 3910 "garage s3"
check_port 55433 "postgres"
check_port 1026 "mailpit smtp"
check_port 8026 "mailpit ui/api"

# -- host disk space ---------------------------------------------------------
echo "-- disk space --"
step "reading df for /"
host_avail_kb=$(with_timeout 5 df -k / | awk 'NR==2 {print $4}')
if [ -z "$host_avail_kb" ]; then
	warn "df did not answer within 5s — host free disk unchecked (a stalled network mount?)"
else
	host_avail_gb=$((host_avail_kb / 1024 / 1024))
	if [ "$host_avail_gb" -lt 20 ]; then
		warn "host free disk: ${host_avail_gb} GB (< 20 GB — make test-big/test-50g may fail)"
	else
		pass "host free disk: ${host_avail_gb} GB"
	fi
fi

# -- Docker VM free disk + host<->VM clock skew ------------------------------
# One throwaway container gives us both numbers from the same run.
echo "-- docker VM clock + disk --"
if [ "$docker_up" -eq 1 ]; then
	# The image is checked for before it is used. `docker run` pulls silently
	# when the image is missing, which on a slow or offline network is a wait
	# with no output and no bound — the shape of the three-minute hang.
	probe_image=alpine
	step "looking for the $probe_image probe image"
	if ! with_timeout 10 docker image inspect "$probe_image" >/dev/null 2>&1; then
		info "$probe_image is not present; pulling it (first run only, 60s limit)"
		if ! with_timeout 60 docker pull -q "$probe_image" >/dev/null 2>&1; then
			warn "could not pull $probe_image within 60s — clock skew and VM disk unchecked (offline?)"
			probe_image=""
		fi
	fi

	if [ -n "$probe_image" ]; then
		step "running one throwaway container for the VM's clock and disk (20s limit)"
		host_before=$(date -u +%s)
		vm_out=$(with_timeout 20 docker run --rm "$probe_image" sh -c 'date -u +%s; df -k / | awk "NR==2{print \$4}"' 2>/dev/null)
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
			warn "the throwaway container produced nothing within 20s — clock skew and VM disk unchecked"
		fi
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
elapsed=$(($(date +%s) - started))
if [ "$fail" -ne 0 ]; then
	echo "doctor: FAIL in ${elapsed}s — fix the above before continuing"
	exit 1
fi
echo "doctor: PASS in ${elapsed}s"
