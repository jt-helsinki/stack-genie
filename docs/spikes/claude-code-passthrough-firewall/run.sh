#!/usr/bin/env bash
#
# Spike: Claude-Code subscription-OAuth through LiteLLM's Anthropic pass-through,
# with the tool_permission firewall still blocking a destructive tool call.
#
# Stands up a mock Anthropic upstream (host python3) + a LiteLLM container on an
# isolated docker network, runs experiments E1-E3, prints results, tears down.
# Re-runnable. Never touches real aip-* containers/networks.

set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NET="spike-passthrough-net"
LITELLM="spike-litellm"
LITELLM_IMAGE="ghcr.io/berriai/litellm:latest"
HOST_PORT="4111"          # host -> litellm :4000
MOCK_PORT="8899"          # host python mock
BASE="http://localhost:${HOST_PORT}"
MASTER_KEY="sk-spike-litellm-master"
OAUTH="SPIKE_OAUTH_TOKEN_123"     # the agent's OWN subscription OAuth bearer
REQ_LOG="${HERE}/requests.jsonl"
MOCK_PID=""

hr()   { printf '\n============================================================\n%s\n============================================================\n' "$1"; }
sub()  { printf '\n----- %s -----\n' "$1"; }

cleanup() {
  hr "TEARDOWN"
  docker rm -f "${LITELLM}" >/dev/null 2>&1 && echo "removed ${LITELLM}"
  docker network rm "${NET}" >/dev/null 2>&1 && echo "removed network ${NET}"
  if [[ -n "${MOCK_PID}" ]] && kill -0 "${MOCK_PID}" >/dev/null 2>&1; then
    kill "${MOCK_PID}" >/dev/null 2>&1 && echo "stopped mock (pid ${MOCK_PID})"
  fi
  # belt-and-braces: kill any stray mock on our port
  pkill -f "mock-upstream.py" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# ---- pre-clean any prior run --------------------------------------------------
docker rm -f "${LITELLM}" >/dev/null 2>&1 || true
docker network rm "${NET}" >/dev/null 2>&1 || true
pkill -f "mock-upstream.py" >/dev/null 2>&1 || true
sleep 1

hr "SETUP"
docker network create "${NET}" >/dev/null && echo "created network ${NET}"

sub "start mock upstream (host python3)"
MOCK_PORT="${MOCK_PORT}" python3 "${HERE}/mock-upstream.py" >"${HERE}/mock.log" 2>&1 &
MOCK_PID=$!
sleep 1
if ! kill -0 "${MOCK_PID}" >/dev/null 2>&1; then
  echo "ERROR: mock failed to start"; cat "${HERE}/mock.log"; exit 1
fi
echo "mock pid ${MOCK_PID}, listening on :${MOCK_PORT}"

sub "start litellm container"
docker run -d --name "${LITELLM}" \
  --network "${NET}" \
  --add-host host.docker.internal:host-gateway \
  -p "${HOST_PORT}:4000" \
  -v "${HERE}/litellm-config.yaml:/app/config.yaml:ro" \
  "${LITELLM_IMAGE}" \
  --config /app/config.yaml --port 4000 --detailed_debug >/dev/null
echo "waiting for litellm to become ready..."
ready=""
for i in $(seq 1 60); do
  code=$(curl -s -o /dev/null -w '%{http_code}' "${BASE}/health/liveliness" 2>/dev/null)
  if [[ "${code}" == "200" ]]; then ready="yes"; break; fi
  sleep 2
done
if [[ -z "${ready}" ]]; then
  echo "ERROR: litellm not ready. Last 60 log lines:"; docker logs --tail 60 "${LITELLM}"; exit 1
fi
echo "litellm ready."

# helper: fetch what the mock last saw (bounded so it can never wedge the run)
last_headers() { curl -s --max-time 5 "http://localhost:${MOCK_PORT}/_last"; }

################################################################################
hr "E1 - HEADER FORWARDING: does the agent's OAuth reach the upstream?"
################################################################################

sub "E1a: send ONLY Authorization: Bearer <OAUTH> (no litellm key, authed route)"
echo "\$ curl ${BASE}/anthropic/v1/messages -H 'Authorization: Bearer ${OAUTH}'"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E1a" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

sub "E1b: litellm key on Authorization + OAUTH on x-pass-authorization (the pattern)"
echo "\$ curl ${BASE}/anthropic/v1/messages -H 'Authorization: Bearer <MASTER>' -H 'x-pass-authorization: Bearer ${OAUTH}'"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER_KEY}" \
  -H "x-pass-authorization: Bearer ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E1b" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

sub "E1c: litellm key on Authorization + OAUTH on x-api-key (forward_llm_provider_auth_headers)"
echo "\$ curl ${BASE}/anthropic/v1/messages -H 'Authorization: Bearer <MASTER>' -H 'x-api-key: ${OAUTH}'"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER_KEY}" \
  -H "x-api-key: ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E1c" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

################################################################################
hr "E2 - AUTHORIZATION COLLISION: does the pass-through require a litellm key?"
################################################################################

sub "E2a: authed route, NO Authorization at all (expect 401 from litellm)"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E2a" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

sub "E2b: authed route, WRONG litellm key (expect 401)"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer sk-totally-wrong" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E2b" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'

sub "E2c: UNAUTHENTICATED route /open, only Authorization: Bearer <OAUTH>"
echo "does the raw Authorization leak upstream when litellm isn't authing this route?"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/open/v1/messages" \
  -H "Authorization: Bearer ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E2c" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
echo ">> mock last saw:"; last_headers | python3 -m json.tool 2>/dev/null || last_headers

################################################################################
hr "E3 - FIREWALL: tool_permission blocks the destructive tool_use in the RESPONSE"
################################################################################

sub "E3a: BENIGN tool_use (Bash 'ls -la') -> expect ALLOWED (passes through)"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER_KEY}" \
  -H "x-pass-authorization: Bearer ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E3a" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_BENIGN_TOOL please list files"}]}'

sub "E3b: DESTRUCTIVE tool_use (Bash 'rm -rf /') -> expect BLOCKED by firewall"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/anthropic/v1/messages" \
  -H "Authorization: Bearer ${MASTER_KEY}" \
  -H "x-pass-authorization: Bearer ${OAUTH}" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E3b" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_DESTRUCTIVE clean everything"}]}'

sub "E3c: DESTRUCTIVE on the UNAUTHENTICATED /open route -> does firewall still run?"
curl -s -w '\n[http_status=%{http_code}]\n' "${BASE}/open/v1/messages" \
  -H "content-type: application/json" \
  -H "x-spike-experiment: E3c" \
  -d '{"model":"claude-x","max_tokens":64,"messages":[{"role":"user","content":"SCENARIO_DESTRUCTIVE clean everything"}]}'

################################################################################
hr "RECORDED UPSTREAM REQUESTS (proof of what the mock actually received)"
################################################################################
if [[ -f "${REQ_LOG}" ]]; then
  python3 - "$REQ_LOG" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    rec = json.loads(line)
    hdr = {k.lower(): v for k, v in rec["headers"].items()}
    exp = hdr.get("x-spike-experiment", "?")
    print(f"[{exp}] path={rec['path']}")
    print(f"    authorization        = {hdr.get('authorization','<none>')!r}")
    print(f"    x-api-key            = {hdr.get('x-api-key','<none>')!r}")
    print(f"    x-pass-authorization = {hdr.get('x-pass-authorization','<none>')!r}")
PY
else
  echo "(no upstream requests recorded - nothing reached the mock)"
fi

hr "DONE (teardown runs next via trap)"
