"""Reference OpenAI-compatible agent image for agent-orca (`ghcr.io/heddles/agent-orca/openai-reference`).

A minimal, framework-agnostic transport — NOT a persona. It streams a chat request to the
model-router and prints/returns the tokens. The model-router injects everything else:
`systemPrompt`, `tools`, episodic summaries, long-term-memory context, prior conversation
history, guardrail filtering, tool-call resolution (custom/MCP/built-ins like `_done`,
`_rag_search`, `_spawn`, …), spend accounting, and checkpointing. So this image only ships:

    input  ->  POST $OPENAI_BASE_URL/chat/completions  ->  output

Two input modes (the operator injects `AGENTORC_INPUT` only in `env` mode, so presence is
the signal):

  * **env** (default): `AGENTORC_INPUT` is set -> run one turn against the model-router and exit.
  * **http**: no `AGENTORC_INPUT` -> serve an HTTP server on `PORT` (default 8000) with:
        GET  /healthz         -> 200 ok            (readiness)
        POST /invoke {"input"} -> 200 {"output":…}  (per-message execution)

The `agentorca` Python SDK is installed in this image for users who want richer behavior
(custom `@agent.tool` registration, lifecycle methods, checkpoint helpers) — replace this
file with your own SDK-based agent to customize; see docs/integrating.md and
docs/agent-images.md.

Deploy an Agent with `framework: openai-compatible`; the operator injects
`OPENAI_BASE_URL` (http://<model-router>:8080/v1) and `OPENAI_API_KEY`.
"""

import json
import os
import sys
import urllib.error
import urllib.request

BASE_URL = os.environ.get("OPENAI_BASE_URL", "http://localhost:8080").rstrip("/")
if not BASE_URL.endswith("/v1"):
    BASE_URL = f"{BASE_URL}/v1"
ENDPOINT = f"{BASE_URL}/chat/completions"
API_KEY = os.environ.get("OPENAI_API_KEY", "")
TIMEOUT = int(os.environ.get("AGENTORC_TIMEOUT_SEC", "600"))


def _stream(input_text: str) -> str:
    """POST a streaming chat-completion request; print tokens and return the full output."""
    body = json.dumps({
        "model": "default",
        "stream": True,
        "messages": [{"role": "user", "content": input_text}],
    }).encode("utf-8")
    headers = {"Content-Type": "application/json"}
    if API_KEY:
        headers["Authorization"] = f"Bearer {API_KEY}"
    req = urllib.request.Request(ENDPOINT, data=body, headers=headers)
    output = ""
    with urllib.request.urlopen(req, timeout=TIMEOUT) as resp:
        for line in resp:
            line = line.decode("utf-8", "replace").strip()
            if not line.startswith("data: "):
                continue
            data = line[6:]
            if data == "" or data == "[DONE]":
                break
            try:
                chunk = json.loads(data)
            except ValueError:
                continue
            choices = chunk.get("choices", [])
            if not choices:
                continue
            content = choices[0].get("delta", {}).get("content", "")
            if content:
                output += content
                print(content, end="", flush=True)
    print(flush=True)
    return output


def run_once(input_text: str) -> str:
    """Run a single turn; re-raise transport errors so callers decide (env mode exits,
    http mode returns 500). Printed for log visibility."""
    try:
        return _stream(input_text)
    except urllib.error.HTTPError as e:
        print(f"HTTP Error {e.code}: {e.reason}", flush=True)
        raise
    except Exception as e:  # noqa: BLE001
        print(f"Error: {e}", flush=True)
        raise


def env_mode() -> None:
    inp = os.environ.get("AGENTORC_INPUT", "")
    if not inp and "AGENTORC_INPUT" not in os.environ:
        print("No AGENTORC_INPUT set and inputMode is env", flush=True)
        sys.exit(1)
    try:
        run_once(inp)
    except Exception:
        sys.exit(1)


def http_mode() -> None:
    from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

    port = int(os.environ.get("PORT", "8000"))

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            if self.path == "/healthz":
                _respond(self, 200, "ok")
            else:
                _respond(self, 404, {"error": "not found"})

        def do_POST(self):
            if self.path != "/invoke":
                _respond(self, 404, {"error": "not found"})
                return
            length = int(self.headers.get("Content-Length", "0"))
            raw = self.rfile.read(length)
            try:
                payload = json.loads(raw) if raw else {}
            except ValueError:
                _respond(self, 400, {"error": "invalid JSON"})
                return
            try:
                output = run_once(payload.get("input", ""))
            except Exception:  # noqa: BLE001
                _respond(self, 500, {"error": "upstream error"})
                return
            _respond(self, 200, {"output": output})

    print(f"reference agent listening on :{port}", flush=True)
    ThreadingHTTPServer(("", port), Handler).serve_forever()


def _respond(handler, status, body):
    """Write a JSON (or plain-text) HTTP response."""
    if isinstance(body, str):
        data = body.encode("utf-8")
        ctype = "text/plain"
    else:
        data = json.dumps(body).encode("utf-8")
        ctype = "application/json"
    handler.send_response(status)
    handler.send_header("Content-Type", ctype)
    handler.send_header("Content-Length", str(len(data)))
    handler.end_headers()
    handler.wfile.write(data)


if __name__ == "__main__":
    # env mode is signaled by the operator injecting AGENTORC_INPUT.
    if "AGENTORC_INPUT" in os.environ:
        env_mode()
    else:
        http_mode()
