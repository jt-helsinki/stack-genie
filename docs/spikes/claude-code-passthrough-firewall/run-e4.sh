#!/usr/bin/env bash
#
# E4: does LiteLLM's PROVIDER-NATIVE Anthropic passthrough (/anthropic/*) run the
# tool_permission firewall AND forward the caller's OAuth?  (The generic
# pass_through_endpoints route failed E3 - firewall never inspected the body.)
#
# Reuses mock-upstream.py. Redirects the native Anthropic upstream to the mock via
# ANTHROPIC_API_BASE. Isolated network + spike- containers; cleans up on exit.

set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NET="spike-e4-net"; LITELLM="spike-e4-litellm"; IMG="ghcr.io/berriai/litellm:latest"
HOST_PORT="4113"; MOCK_PORT="8897"; BASE="http://localhost:${HOST_PORT}"
MASTER="sk-spike-litellm-master"; OAUTH="SPIKE_OAUTH_TOKEN_123"; ANTH_KEY="sk-ant-dummy-spike"
REQ_LOG="${HERE}/requests.jsonl"; MOCK_PID=""

hr(){ printf '\n============================================================\n%s\n============================================================\n' "$1"; }
sub(){ printf '\n----- %s -----\n' "$1"; }
cleanup(){ hr "TEARDOWN"; docker rm -f "$LITELLM" >/dev/null 2>&1 && echo "removed $LITELLM"
  docker network rm "$NET" >/dev/null 2>&1 && echo "removed $NET"
  [[ -n "$MOCK_PID" ]] && kill "$MOCK_PID" >/dev/null 2>&1 && echo "stopped mock $MOCK_PID"
  pkill -f "mock-upstream.py" >/dev/null 2>&1 || true; }
trap cleanup EXIT INT TERM

docker rm -f "$LITELLM" >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true
pkill -f "mock-upstream.py" >/dev/null 2>&1 || true; sleep 1

hr "SETUP (E4 - native anthropic passthrough)"
docker network create "$NET" >/dev/null && echo "created $NET"
MOCK_PORT="$MOCK_PORT" python3 "$HERE/mock-upstream.py" >"$HERE/mock.log" 2>&1 &
MOCK_PID=$!; sleep 1
kill -0 "$MOCK_PID" 2>/dev/null || { echo "mock failed"; cat "$HERE/mock.log"; exit 1; }
echo "mock pid $MOCK_PID on :$MOCK_PORT"

# ANTHROPIC_API_BASE overrides the native passthrough upstream; DISABLE_URL_SUFFIX
# stops LiteLLM appending an extra path suffix so /anthropic/v1/messages maps to
# <base>/v1/messages == the mock's path.
docker run -d --name "$LITELLM" --network "$NET" \
  --add-host host.docker.internal:host-gateway -p "${HOST_PORT}:4000" \
  -e ANTHROPIC_API_KEY="$ANTH_KEY" \
  -e ANTHROPIC_API_BASE="http://host.docker.internal:${MOCK_PORT}" \
  -e LITELLM_ANTHROPIC_DISABLE_URL_SUFFIX="true" \
  -v "${HERE}/litellm-config-native.yaml:/app/config.yaml:ro" \
  "$IMG" --config /app/config.yaml --port 4000 --detailed_debug >/dev/null
echo "waiting for litellm..."
for i in $(seq 1 60); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' ${BASE}/health/liveliness 2>/dev/null)" = "200" ] && { echo ready; break; }
  sleep 2
done

last_headers(){ curl -s --max-time 5 "http://localhost:${MOCK_PORT}/_last"; }

hr "E4a - BENIGN Bash 'ls -la' via NATIVE /anthropic/v1/messages -> expect ALLOWED"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER}" -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" -H "x-spike-experiment: E4a" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_BENIGN_TOOL list files"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

hr "E4b - DESTRUCTIVE Bash 'rm -rf /' via NATIVE route -> expect BLOCKED if firewall fires"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER}" -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" -H "x-spike-experiment: E4b" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_DESTRUCTIVE clean everything"}]}'

hr "E4c - OAUTH FORWARDING: send Authorization: Bearer <OAUTH> only (client's own creds)"
echo "on the native route, is the caller's OAuth forwarded, or does litellm substitute ANTHROPIC_API_KEY / require its own key?"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${OAUTH}" -H "content-type: application/json" \
  -H "anthropic-version: 2023-06-01" -H "x-spike-experiment: E4c" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_BENIGN_TOOL hi"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

hr "RECORDED UPSTREAM REQUESTS (what the mock actually received)"
if [[ -f "$REQ_LOG" ]]; then
  python3 - "$REQ_LOG" <<'PY'
import json,sys
for line in open(sys.argv[1]):
    r=json.loads(line); h={k.lower():v for k,v in r["headers"].items()}
    print(f"[{h.get('x-spike-experiment','?')}] path={r['path']}")
    print(f"    authorization = {h.get('authorization','<none>')!r}")
    print(f"    x-api-key     = {h.get('x-api-key','<none>')!r}")
PY
else echo "(nothing reached the mock)"; fi

hr "GUARDRAIL EXECUTION TRACES (did tool_permission.py inspect the body, or only init?)"
docker logs "$LITELLM" 2>&1 | grep -iE "tool_permission\.py|Tool Permission|denied|blocked|SPIKE-FIREWALL|should_run_guardrail for guardrail=tool-firewall.*post_call|anthropic.*passthrough|AnthropicPassthrough" | cut -c1-260 | tail -40

hr "DONE"
