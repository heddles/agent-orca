"""
Minimal MCP server implementing JSON-RPC 2.0 over HTTP.

Tools provided:
  - current_time: Return the current UTC time
  - random_number: Generate a random integer in a given range
  - dns_lookup: Resolve a hostname to IP addresses
  - reverse: Reverse a string

These tools return live data that an LLM cannot fabricate,
making them ideal for verifying end-to-end MCP tool invocation.

No external dependencies — uses only the Python 3 stdlib.
"""

import json
import random
import socket
import sys
from datetime import datetime, timezone
from http.server import HTTPServer, BaseHTTPRequestHandler

TOOLS = [
    {
        "name": "current_time",
        "description": "Return the current date and time in UTC. The LLM cannot know the real time without calling this tool.",
        "inputSchema": {
            "type": "object",
            "properties": {},
        },
    },
    {
        "name": "random_number",
        "description": "Generate a cryptographically unpredictable random integer between min and max (inclusive). The LLM cannot produce truly random numbers without calling this tool.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "min": {"type": "integer", "description": "Lower bound (inclusive)", "default": 1},
                "max": {"type": "integer", "description": "Upper bound (inclusive)", "default": 100},
            },
        },
    },
    {
        "name": "dns_lookup",
        "description": "Resolve a hostname to its IP addresses using the cluster DNS. The LLM cannot perform live DNS resolution without calling this tool.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "hostname": {"type": "string", "description": "The hostname to resolve"},
            },
            "required": ["hostname"],
        },
    },
    {
        "name": "reverse",
        "description": "Reverse a string and return it. Useful for verifying the tool was actually called since LLMs often struggle with character-level string manipulation.",
        "inputSchema": {
            "type": "object",
            "properties": {
                "text": {"type": "string", "description": "The string to reverse"},
            },
            "required": ["text"],
        },
    },
]


def handle_tool_call(name: str, arguments: dict) -> dict:
    """Execute a tool and return MCP content response."""
    if name == "current_time":
        now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        return {"content": [{"type": "text", "text": now}], "isError": False}

    if name == "random_number":
        lo = arguments.get("min", 1)
        hi = arguments.get("max", 100)
        result = random.randint(lo, hi)
        return {"content": [{"type": "text", "text": str(result)}], "isError": False}

    if name == "dns_lookup":
        hostname = arguments.get("hostname", "")
        try:
            ips = socket.getaddrinfo(hostname, None)
            unique = sorted(set(addr[4][0] for addr in ips))
            return {"content": [{"type": "text", "text": ", ".join(unique)}], "isError": False}
        except socket.gaierror as e:
            return {"content": [{"type": "text", "text": f"DNS error: {e}"}], "isError": True}

    if name == "reverse":
        text = arguments.get("text", "")
        return {"content": [{"type": "text", "text": text[::-1]}], "isError": False}

    return {
        "content": [{"type": "text", "text": f"Unknown tool: {name}"}],
        "isError": True,
    }


class MCPHandler(BaseHTTPRequestHandler):
    """Handle MCP JSON-RPC 2.0 requests over HTTP POST."""

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}

        method = body.get("method", "")
        req_id = body.get("id")
        params = body.get("params") or {}

        if method == "initialize":
            result = {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {"listChanged": False}},
                "serverInfo": {"name": "demo-mcp-server", "version": "0.2.0"},
            }
        elif method == "notifications/initialized":
            self._send_json(204, None)
            return
        elif method == "tools/list":
            result = {"tools": TOOLS}
        elif method == "tools/call":
            tool_name = params.get("name", "")
            tool_args = params.get("arguments", {})
            print(f"[tools/call] {tool_name}({json.dumps(tool_args)})", file=sys.stderr, flush=True)
            result = handle_tool_call(tool_name, tool_args)
            print(f"[tools/call] -> {json.dumps(result)}", file=sys.stderr, flush=True)
        else:
            self._send_json(200, {
                "jsonrpc": "2.0",
                "id": req_id,
                "error": {"code": -32601, "message": f"Method not found: {method}"},
            })
            return

        self._send_json(200, {"jsonrpc": "2.0", "id": req_id, "result": result})

    def _send_json(self, status: int, payload):
        if payload is None:
            self.send_response(status)
            self.end_headers()
            return
        data = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, fmt, *args):
        print(f"[mcp-server] {fmt % args}", file=sys.stderr, flush=True)


if __name__ == "__main__":
    port = 3000
    print(f"[mcp-server] Listening on :{port}", file=sys.stderr, flush=True)
    HTTPServer(("", port), MCPHandler).serve_forever()
