#!/usr/bin/env python3
"""
network-mcp-server: MCP HTTP transport server for safe network reconnaissance.
Provides tools for target scanning and host enumeration.
"""
import json, os, socket, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

_CSS_BASE = (
    "*{box-sizing:border-box;margin:0;padding:0}"
    "body{background:#0f172a;color:#e2e8f0;"
    "font-family:'Inter',system-ui,-apple-system,sans-serif;"
    "font-size:13px;padding:16px;line-height:1.6}"
    "h1{font-size:16px;font-weight:700;color:#f1f5f9;letter-spacing:.02em}"
    ".ts{color:#64748b;font-size:11px;margin-bottom:14px;margin-top:2px}"
    ".label{color:#64748b;font-size:10px;text-transform:uppercase;letter-spacing:.06em;margin-bottom:5px}"
    ".card{background:#1e293b;border:1px solid #334155;border-radius:6px;padding:12px 14px;margin-bottom:8px}"
    ".tag{display:inline-block;padding:2px 7px;border-radius:3px;font-size:10px;font-weight:700;"
    "letter-spacing:.04em;text-transform:uppercase;margin-right:4px;margin-bottom:3px}"
    ".port-open{color:#4ade80}"
    ".port-closed{color:#94a3b8}"
    ".service-badge{background:rgba(96,165,250,.15);color:#60a5fa}"
)

TOOLS = [
    {
        "name": "scan-target",
        "description": (
            "Perform safe port scan and service detection on target host. "
            "Respects rate limits and only scans non-destructive ports."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string", "description": "Target hostname or IP address"},
                "ports": {"type": "string", "default": "22,80,443,8080,8443",
                          "description": "Comma-separated ports or range (e.g. 1-1024)"},
                "safe_only": {"type": "boolean", "default": True},
            },
            "required": ["target"],
        },
        "_meta": {"ui": {"resourceUri": "network://scan-results"}},
    },
    {
        "name": "enumerate-hosts",
        "description": (
            "Discover live hosts on a subnet using ICMP ping sweep. "
            "Limited to /24 subnets for safety."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "subnet": {"type": "string", "pattern": "^\\d{1,3}\\.\\d{1,3}\\.\\d{1,3}\\.\\d{1,3}/24$"},
            },
            "required": ["subnet"],
        },
    },
]

# Common service fingerprints
SERVICE_FINGERPRINTS = {
    22: "SSH",
    80: "HTTP",
    443: "HTTPS",
    21: "FTP",
    23: "Telnet",
    25: "SMTP",
    53: "DNS",
    110: "POP3",
    143: "IMAP",
    3306: "MySQL",
    5432: "PostgreSQL",
    6379: "Redis",
    27017: "MongoDB",
    8080: "HTTP-Alt",
    8443: "HTTPS-Alt",
}

# Simulated scan results (for demo without actual network access)
SIMULATED_TARGETS = {
    "web-victim.example.com": [
        {"port": 22, "status": "open", "service": "SSH", "version": "OpenSSH 9.2"},
        {"port": 80, "status": "open", "service": "HTTP", "version": "Apache 2.4.52"},
        {"port": 443, "status": "open", "service": "HTTPS", "version": "Apache 2.4.52"},
        {"port": 8080, "status": "open", "service": "HTTP-Alt", "version": "Tomcat 8.5.19"},
        {"port": 8443, "status": "closed", "service": None, "version": None},
    ],
    "api-victim.example.com": [
        {"port": 22, "status": "filtered", "service": None},
        {"port": 80, "status": "closed", "service": None},
        {"port": 443, "status": "open", "service": "HTTPS", "version": "nginx 1.24.0"},
        {"port": 3000, "status": "open", "service": "HTTP", "version": "Node.js Express"},
    ],
    "db-victim.example.com": [
        {"port": 22, "status": "open", "service": "SSH", "version": "OpenSSH 8.9"},
        {"port": 3306, "status": "open", "service": "MySQL", "version": "8.0.33"},
        {"port": 5432, "status": "open", "service": "PostgreSQL", "version": "15.2"},
    ],
}


def parse_ports(ports_str):
    """Parse port string into list of integers."""
    ports = []
    for part in ports_str.split(","):
        part = part.strip()
        if "-" in part:
            start, end = part.split("-")
            ports.extend(range(int(start), int(end) + 1))
        else:
            ports.append(int(part))
    return ports


def scan_target(target, ports="22,80,443,8080,8443", safe_only=True):
    """Perform simulated port scan on target."""
    # In demo mode, return simulated results
    if target in SIMULATED_TARGETS:
        port_list = parse_ports(ports)
        return {
            "target": target,
            "scan_time": datetime.now(timezone.utc).isoformat(),
            "ports_scanned": port_list if ports != "common" else list(SERVICE_FINGERPRINTS.keys()),
            "results": [r for r in SIMULATED_TARGETS[target] if r["port"] in port_list],
        }

    # Real scan simulation for unknown targets
    return {
        "target": target,
        "scan_time": datetime.now(timezone.utc).isoformat(),
        "error": "Target not in simulated database. In production, this would perform actual scanning.",
        "suggestion": "Try a demo target like 'web-victim.example.com'",
    }


def enumerate_hosts(subnet):
    """Discover live hosts on subnet (simulated)."""
    base = ".".join(subnet.split("/")[:3])
    return {
        "subnet": subnet,
        "live_hosts": [
            f"{base}.10",
            f"{base}.20",
            f"{base}.50",
        ],
        "hosts": [
            {"ip": f"{base}.10", "hostname": "web-victim.example.com", "ports": [22, 80, 443, 8080]},
            {"ip": f"{base}.20", "hostname": "api-victim.example.com", "ports": [443, 3000]},
            {"ip": f"{base}.50", "hostname": "db-victim.example.com", "ports": [22, 3306, 5432]},
        ],
    }


def scan_results_html(result):
    """Render scan results as interactive HTML table."""
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
    target = result.get("target", "unknown")
    results = result.get("results", [])

    if "error" in result:
        return f"""<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>Scan Error</title>
<style>{_CSS_BASE}</style></head>
<body>
<h1>Scan Error</h1>
<div class="card">{result['error']}</div>
<p style="color:#64748b;margin-top:8px">{result.get('suggestion', '')}</p>
</body>
</html>"""

    rows = ""
    for r in results:
        status_class = "port-open" if r["status"] == "open" else "port-closed"
        service_badge = ""
        if r.get("service"):
            service_badge = f"<span class='tag service-badge'>{r['service']}</span>"
        if r.get("version"):
            service_badge += f" <span style='color:#94a3b8'>{r['version']}</span>"
        rows += f"""
        <tr>
            <td>{r['port']}</td>
            <td><span class='{status_class}'>{r['status']}</span></td>
            <td>{service_badge}</td>
        </tr>"""

    if not rows:
        rows = "<tr><td colspan='3' style='color:#64748b;text-align:center'>No open ports found</td></tr>"

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Nmap Scan: {target}</title>
<style>{_CSS_BASE}</style>
</head>
<body>
<h1>Port Scan Results</h1>
<div class="ts">Target: <code>{target}</code> • {ts}</div>
<table>
<thead>
<tr><th style='width:60px'>Port</th><th style='width:80px'>Status</th><th>Service</th></tr>
</thead>
<tbody>
{rows}
</tbody>
</table>
</body>
</html>"""


_last_scan_result = {}


def handle_rpc(method, params, req_id):
    global _last_scan_result
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}, "resources": {}},
            "serverInfo": {"name": "network-mcp-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}

        if name == "scan-target":
            target = args.get("target", "")
            ports = args.get("ports", "22,80,443,8080,8443")
            safe_only = args.get("safe_only", True)
            result = scan_target(target, ports, safe_only)
            _last_scan_result = result
            return {"content": [{"type": "text", "text": json.dumps(result, indent=2)}], "isError": False}, req_id

        if name == "enumerate-hosts":
            subnet = args.get("subnet", "")
            result = enumerate_hosts(subnet)
            return {"content": [{"type": "text", "text": json.dumps(result, indent=2)}], "isError": False}, req_id

        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "network://scan-results":
            html = scan_results_html(_last_scan_result)
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": html}]}, req_id
        return {"error": {"code": -32002, "message": "Resource not found: " + uri}}, req_id

    return {"error": {"code": -32601, "message": "Method not found: " + method}}, req_id


class MCPHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"ok")
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._respond({"jsonrpc": "2.0", "id": None,
                           "error": {"code": -32700, "message": "Parse error"}})
            return
        result, resp_id = handle_rpc(req.get("method", ""), req.get("params"), req.get("id"))
        if result is None and resp_id is None:
            self.send_response(204)
            self.end_headers()
            return
        if isinstance(result, dict) and "error" in result and len(result) == 1:
            self.send_response(500)
            self.end_headers()
            self._respond(result)
            return
        self.send_response(200)
        self.end_headers()
        self._respond({"jsonrpc": "2.0", "id": resp_id, "result": result})

    def _respond(self, data):
        self.wfile.write(json.dumps(data).encode())


if __name__ == "__main__":
    HTTPServer(("0.0.0.0", PORT), MCPHandler).serve_forever()