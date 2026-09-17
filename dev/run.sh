#!/bin/sh
# dev/run.sh — run prober-backend and prober-agent locally over TLS for `make
# dev`. Grants the agent CAP_NET_RAW, starts both, and shuts them down cleanly
# on Ctrl+C.
set -e
cd "$(dirname "$0")/.."

./dev/gen-certs.sh

# The agent opens raw ICMP sockets, which need CAP_NET_RAW. Grant it to the
# built binary (idempotent). Root needs nothing; otherwise use sudo+setcap,
# and if either is missing, say how to do it by hand and carry on so the
# backend still comes up.
if [ "$(id -u)" != 0 ]; then
	if command -v setcap >/dev/null 2>&1 && command -v sudo >/dev/null 2>&1; then
		echo ">> granting CAP_NET_RAW to ./prober-agent (sudo may prompt)"
		sudo setcap cap_net_raw+ep ./prober-agent \
			|| echo "!! setcap failed; the agent will not open raw sockets"
	else
		echo "!! setcap or sudo missing; grant CAP_NET_RAW by hand or run 'make dev' as root:"
		echo "     sudo setcap cap_net_raw+ep ./prober-agent"
	fi
fi

BE="" AG=""
cleanup() {
	[ -n "$AG" ] && kill "$AG" 2>/dev/null || true
	[ -n "$BE" ] && kill "$BE" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

echo ">> backend on :8080 (http) :50051 (grpc) :9108 (metrics); agent site=local"
echo ">> open http://127.0.0.1:8080  (Ctrl+C to stop)"
./prober-backend -config dev/backend.yml & BE=$!
sleep 1
./prober-agent -config dev/agent.yml & AG=$!

# If the agent dies immediately (commonly a missing CAP_NET_RAW), say so
# plainly rather than leaving the backend up with nothing tracing.
sleep 1
if ! kill -0 "$AG" 2>/dev/null; then
	echo "!! prober-agent exited right after start."
	echo "   Most likely it could not open raw sockets (needs CAP_NET_RAW)."
	echo "   Grant it and re-run:  sudo setcap cap_net_raw+ep ./prober-agent"
	echo "   The backend is still up on :8080; press Ctrl+C to stop."
fi

# Wait for whichever exits first, then cleanup runs via the trap.
wait
