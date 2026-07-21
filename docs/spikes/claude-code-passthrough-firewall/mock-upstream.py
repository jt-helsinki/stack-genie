#!/usr/bin/env python3
"""
Mock "Anthropic" upstream for the LiteLLM pass-through + firewall spike.

- Accepts POST /v1/messages (the Anthropic Messages API path).
- RECORDS every request header it received to requests.jsonl (one JSON object
  per line) AND prints a compact line to stdout, so we can PROVE what
  Authorization / x-api-key the upstream actually saw.
- Returns a canned Anthropic Messages JSON response. The response VARIES by a
  marker in the request body so we can drive the three experiments:
    * body contains "SCENARIO_DESTRUCTIVE"  -> tool_use Bash `rm -rf /`
    * body contains "SCENARIO_BENIGN_TOOL"  -> tool_use Bash `ls -la`
    * otherwise                             -> plain text (no tool_use)
- GET /_last returns the last recorded request's headers as JSON (handy for
  fetching proof from the host without reading the file).

Runs on the HOST (python3 http.server); the LiteLLM container reaches it via
host.docker.internal. Bind 0.0.0.0 so the container can connect.
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
LOG_PATH = os.path.join(HERE, "requests.jsonl")
PORT = int(os.environ.get("MOCK_PORT", "8899"))

_last = {"headers": {}, "path": None, "body": None}


def _canned_response(body_text: str) -> dict:
    base = {
        "id": "msg_spike_0001",
        "type": "message",
        "role": "assistant",
        "model": "claude-mock-spike",
        "stop_reason": "tool_use",
        "usage": {"input_tokens": 10, "output_tokens": 20},
    }
    if "SCENARIO_DESTRUCTIVE" in body_text:
        base["content"] = [
            {"type": "text", "text": "I'll clean up the workspace for you."},
            {
                "type": "tool_use",
                "id": "toolu_destructive_01",
                "name": "Bash",
                "input": {"command": "rm -rf /"},
            },
        ]
    elif "SCENARIO_BENIGN_TOOL" in body_text:
        base["content"] = [
            {"type": "text", "text": "Listing the directory."},
            {
                "type": "tool_use",
                "id": "toolu_benign_01",
                "name": "Bash",
                "input": {"command": "ls -la"},
            },
        ]
    else:
        base["stop_reason"] = "end_turn"
        base["content"] = [
            {"type": "text", "text": "Hello from the mock Anthropic upstream."}
        ]
    return base


class Handler(BaseHTTPRequestHandler):
    # HTTP/1.0 => no keep-alive: connection closes after each response, so a
    # single upstream client can't wedge the server (belt-and-braces alongside
    # ThreadingHTTPServer below).
    protocol_version = "HTTP/1.0"

    def log_message(self, *args):  # silence default noisy logging
        pass

    def _record(self, body_text: str):
        headers = {key: value for key, value in self.headers.items()}
        record = {"path": self.path, "headers": headers, "body": body_text}
        _last.update(headers=headers, path=self.path, body=body_text)
        with open(LOG_PATH, "a") as handle:
            handle.write(json.dumps(record) + "\n")
        auth = headers.get("Authorization", headers.get("authorization", "<none>"))
        xkey = headers.get("x-api-key", headers.get("X-Api-Key", "<none>"))
        xpass = headers.get(
            "x-pass-authorization", headers.get("X-Pass-Authorization", "<none>")
        )
        print(
            f"[mock] {self.path}  Authorization={auth!r}  "
            f"x-api-key={xkey!r}  x-pass-authorization={xpass!r}",
            flush=True,
        )

    def do_GET(self):
        if self.path == "/_last":
            payload = json.dumps(_last).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return
        self.send_response(404)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(length) if length else b""
        body_text = raw.decode("utf-8", errors="replace")
        self._record(body_text)
        payload = json.dumps(_canned_response(body_text)).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


if __name__ == "__main__":
    # fresh log each run
    try:
        os.remove(LOG_PATH)
    except FileNotFoundError:
        pass
    server = ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"[mock] listening on 0.0.0.0:{PORT}, logging to {LOG_PATH}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        sys.exit(0)
