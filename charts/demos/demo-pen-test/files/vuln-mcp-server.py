#!/usr/bin/env python3
"""
vuln-mcp-server: MCP HTTP transport server for vulnerability scanning.
Provides safe, non-destructive vulnerability detection tools.
"""
import json, os
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
    ".severity-critical{background:rgba(239,68,68,.18);color:#f87171;border:1px solid #f87171}"
    ".severity-high{background:rgba(249,115,22,.18);color:#fb923c;border:1px solid #fb923c}"
    ".severity-medium{background:rgba(234,179,8,.15);color:#facc15;border:1px solid #facc15}"
    ".severity-low{background:rgba(74,222,128,.15);color:#4ade80;border:1px solid #4ade80}"
)

TOOLS = [
    {
        "name": "scan-host",
        "description": (
            "Run non-destructive vulnerability checks against a host. "
            "Detects outdated software versions, misconfigurations, and common web vulnerabilities."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string", "description": "Target hostname or IP"},
                "scan_type": {"type": "string", "enum": ["quick", "full", "web"], "default": "quick"},
                "rate_limit": {"type": "integer", "default": 10, "description": "Requests per second"},
            },
            "required": ["target"],
        },
        "_meta": {"ui": {"resourceUri": "vuln://scan-results"}},
    },
]

# Simulated vulnerability findings
VULN_DATABASE = {
    "web-victim.example.com": [
        {
            "id": "VULN-001",
            "cve_id": "CVE-2024-29944",
            "port": 8080,
            "service": "Apache Tomcat",
            "severity": "HIGH",
            "title": "Outdated Apache Tomcat version",
            "description": "Tomcat 8.5.19 has known vulnerabilities including CVE-2024-29944",
            "evidence": "HTTP response header: Server: Apache-Coyote/1.1 (Tomcat 8.5.19)",
            "remediation": "Upgrade to Tomcat 9.0.x or 10.1.x series",
        },
        {
            "id": "VULN-002",
            "cve_id": "CVE-2024-38473",
            "port": 80,
            "service": "Apache HTTPD",
            "severity": "MEDIUM",
            "title": "Potential reflected XSS in user profile",
            "description": "User profile page reflects URL parameters without sanitization",
            "evidence": "GET /profile?name=<script>alert(1)</script> renders script tag",
            "remediation": "Implement output encoding and Content-Security-Policy",
        },
    ],
    "api-victim.example.com": [
        {
            "id": "VULN-003",
            "cve_id": "CVE-2024-45269",
            "port": 443,
            "service": "nginx",
            "severity": "HIGH",
            "title": "Potential auth bypass in reverse proxy",
            "description": "Misconfigured nginx may allow path traversal",
            "evidence": "Location header allows .. traversal in redirect",
            "remediation": "Review nginx location block configuration",
        },
    ],
    "db-victim.example.com": [
        {
            "id": "VULN-004",
            "cve_id": "CVE-2021-44228",
            "port": 3306,
            "service": "MySQL",
            "severity": "CRITICAL",
            "title": "Log4Shell adjacent - JNDI injection vector",
            "description": "Java-based application with Log4j detected",
            "evidence": "Banner indicates Java application stack",
            "remediation": "Apply Log4j security patch immediately",
        },
    ],
}


def scan_host(target, scan_type="quick", rate_limit=10):
    """Perform simulated vulnerability scan on target."""
    findings = []

    if target in VULN_DATABASE:
        findings = VULN_DATABASE[target]
        if scan_type == "quick":
            findings = [f for f in findings if f["severity"] in ("CRITICAL", "HIGH")]
    else:
        findings = [{
            "id": "INFO-001",
            "severity": "INFO",
            "title": "Target not in simulation database",
            "description": f"Try demo targets: web-victim.example.com, api-victim.example.com, db-victim.example.com",
        }]

    return {
        "target": target,
        "scan_type": scan_type,
        "scan_time": datetime.now(timezone.utc).isoformat(),
        "findings": findings,
        "summary": {
            "critical": len([f for f in findings if f.get("severity") == "CRITICAL"]),
            "high": len([f for f in findings if f.get("severity") == "HIGH"]),
            "medium": len([f for f in findings if f.get("severity") == "MEDIUM"]),
            "low": len([f for f in findings if f.get("severity") == "LOW"]),
            "total": len(findings),
        },
    }


def scan_results_html(result):
    """Render vulnerability scan results as interactive HTML."""
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
    target = result.get("target", "unknown")
    findings = result.get("findings", [])
    summary = result.get("summary", {})

    # Summary bar
    summary_html = f"""
    <div style="display:flex;gap:12px;margin-bottom:16px">
        <span class="tag severity-critical">Critical: {summary.get('critical', 0)}</span>
        <span class="tag severity-high">High: {summary.get('high', 0)}</span>
        <span class="tag severity-medium">Medium: {summary.get('medium', 0)}</span>
        <span class="tag severity-low">Low: {summary.get('low', 0)}</span>
        <span style="color:#64748b;margin-left:auto">Total: {summary.get('total', 0)}</span>
    </div>"""

    cards = ""
    for f in findings:
        sev = f.get("severity", "INFO").lower()
        sev_class = f"severity-{sev}"
        cve_link = f"<a href='https://nvd.nist.gov/vuln/detail/{f['cve_id']}' style='color:#60a5fa'>{f['cve_id']}</a>" if f.get('cve_id') else ""
        cards += f"""
        <div class="card">
            <div style="display:flex;align-items:center;gap:8px;margin-bottom:8px">
                <span class="tag {sev_class}">{f.get('severity')}</span>
                <span style="color:#f1f5f9;font-weight:600">{f.get('title')}</span>
                <span style="margin-left:auto;color:#64748b;font-size:11px">{f.get('service')} port {f.get('port', '?')}</span>
            </div>
            <div style="color:#cbd5e1;margin-bottom:6px">{f.get('description')}</div>
            <div class="label">Evidence</div>
            <div style="color:#94a3b8;font-size:11px;margin-top:4px">{f.get('evidence', 'N/A')}</div>
            <div class="label" style="margin-top:8px">Remediation</div>
            <div style="color:#4ade80;font-size:11px;margin-top:4px">{f.get('remediation', 'N/A')}</div>
            {f'<div style="margin-top:8px;color:#64748b;font-size:11px">CVE: {cve_link}</div>' if cve_link else ''}
        </div>"""

    if not cards:
        cards = '<div class="card" style="color:#4ade80">No critical/high vulnerabilities found</div>'

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Vuln Scan: {target}</title>
<style>{_CSS_BASE}</style>
</head>
<body>
<h1>Vulnerability Scan</h1>
<div class="ts">Target: <code>{target}</code> • {ts}</div>
{summary_html}
{cards}
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
            "serverInfo": {"name": "vuln-mcp-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}

        if name == "scan-host":
            target = args.get("target", "")
            scan_type = args.get("scan_type", "quick")
            rate_limit = args.get("rate_limit", 10)
            result = scan_host(target, scan_type, rate_limit)
            _last_scan_result = result
            return {"content": [{"type": "text", "text": json.dumps(result, indent=2)}], "isError": False}, req_id

        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "vuln://scan-results":
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