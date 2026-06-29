#!/usr/bin/env python3
"""
report-mcp-server: MCP HTTP transport server for executive security reports.
Renders interactive dashboards for penetration testing findings.
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
    ".risk-high{background:linear-gradient(135deg,rgba(239,68,68,.3),rgba(249,115,22,.3))}"
    ".risk-medium{background:linear-gradient(135deg,rgba(249,115,22,.3),rgba(234,179,8,.3))}"
    ".risk-low{background:linear-gradient(135deg,rgba(74,222,128,.3),rgba(96,165,250,.3))}"
    ".matrix-grid{display:grid;grid-template-columns:repeat(4,1fr);gap:8px}"
    ".matrix-cell{background:#1e293b;border:1px solid #334155;border-radius:4px;padding:8px;text-align:center}"
)

TOOLS = [
    {
        "name": "render-executive-summary",
        "description": (
            "Render a styled executive report with vulnerability findings and remediation priority. "
            "Creates an interactive dashboard for stakeholders."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string", "description": "Target system scanned"},
                "findings": {
                    "type": "array",
                    "items": {
                        "type": "object",
                        "properties": {
                            "cve_id": {"type": "string"},
                            "severity": {"type": "string", "enum": ["CRITICAL", "HIGH", "MEDIUM", "LOW"]},
                            "title": {"type": "string"},
                            "verified": {"type": "boolean"},
                        },
                    },
                },
                "risk_score": {"type": "number", "description": "Overall risk score 0-100"},
                "executive_summary": {"type": "string", "description": "Executive summary text"},
                "recommendations": {"type": "array", "items": {"type": "string"}},
            },
            "required": ["target", "findings"],
        },
        "_meta": {"ui": {"resourceUri": "report://executive"}},
    },
]

_last_report = {}


def render_executive_summary(target, findings, risk_score=0, executive_summary="", recommendations=None):
    """Store report data for HTML rendering."""
    return {
        "target": target,
        "findings": findings,
        "risk_score": risk_score,
        "executive_summary": executive_summary,
        "recommendations": recommendations or [],
        "generated_at": datetime.now(timezone.utc).isoformat(),
    }


def executive_report_html(report):
    """Render executive report as interactive HTML dashboard."""
    ts = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%S UTC")
    target = report.get("target", "unknown")
    findings = report.get("findings", [])
    risk_score = report.get("risk_score", 0)
    executive_summary = report.get("executive_summary", "")
    recommendations = report.get("recommendations", [])

    # Risk score visualization
    risk_pct = risk_score
    risk_color = "#f87171" if risk_score >= 70 else "#fb923c" if risk_score >= 40 else "#4ade80"
    risk_class = "risk-high" if risk_score >= 70 else "risk-medium" if risk_score >= 40 else "risk-low"

    # Count by severity
    critical = len([f for f in findings if f.get("severity") == "CRITICAL"])
    high = len([f for f in findings if f.get("severity") == "HIGH"])
    medium = len([f for f in findings if f.get("severity") == "MEDIUM"])
    low = len([f for f in findings if f.get("severity") == "LOW"])
    verified = len([f for f in findings if f.get("verified")])

    # Findings cards
    cards = ""
    for f in findings[:10]:  # Limit display
        sev = f.get("severity", "UNKNOWN").lower()
        sev_class = f"severity-{sev}"
        verified_badge = '<span class="tag severity-critical" style="margin-left:8px">VERIFIED</span>' if f.get("verified") else ''
        cve_link = f'<a href="https://nvd.nist.gov/vuln/detail/{f.get("cve_id", "")}" style="color:#60a5fa">{f.get("cve_id", "")}</a>' if f.get("cve_id") else ''
        cards += f"""
        <div class="card">
            <div style="display:flex;align-items:center;gap:8px;margin-bottom:6px">
                <span class="tag {sev_class}">{f.get('severity')}</span>
                <span style="color:#f1f5f9;font-weight:600">{f.get('title')}</span>
                {verified_badge}
            </div>
            {f'<div style="color:#94a3b8;font-size:11px">CVE: {cve_link}</div>' if cve_link else ''}
        </div>"""

    if not cards:
        cards = '<div class="card" style="color:#64748b">No findings to display</div>'

    recs = "".join(f"<li style='margin-bottom:4px'>{r}</li>" for r in recommendations)

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>PenTest Executive Report</title>
<style>{_CSS_BASE}</style>
</head>
<body>
<h1>Penetration Test Report</h1>
<div class="ts">Target: <code>{target}</code> • Generated: {ts}</div>

<!-- Risk score gauge -->
<div style="margin-bottom:16px">
    <div class="label">Overall Risk Score</div>
    <div style="position:relative;height:80px;width:200px">
        <svg viewBox="0 0 100 50" style="width:100%;height:100%">
            <path d="M10,40 A30,30 0 0,1 90,40" stroke="#334155" stroke-width="8" fill="none"/>
            <path d="M10,40 A30,30 0 0,1 {10 + 80 * risk_pct / 100},40" 
                  stroke="{risk_color}" stroke-width="8" fill="none"/>
        </svg>
        <div style="position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);font-size:24px;font-weight:700;color:{risk_color}">
            {risk_pct:.0f}
        </div>
    </div>
</div>

<!-- Summary stats -->
<div class="matrix-grid" style="margin-bottom:16px;max-width:400px">
    <div class="matrix-cell"><div style="font-size:20px;font-weight:700;color:#f87171">{critical}</div><div style="color:#64748b;font-size:10px">Critical</div></div>
    <div class="matrix-cell"><div style="font-size:20px;font-weight:700;color:#fb923c">{high}</div><div style="color:#64748b;font-size:10px">High</div></div>
    <div class="matrix-cell"><div style="font-size:20px;font-weight:700;color:#facc15">{medium}</div><div style="color:#64748b;font-size:10px">Medium</div></div>
    <div class="matrix-cell"><div style="font-size:20px;font-weight:700;color:#4ade80">{verified}</div><div style="color:#64748b;font-size:10px">Verified</div></div>
</div>

<!-- Executive summary -->
<div class="card" style="margin-bottom:12px">
    <div class="label">Executive Summary</div>
    <div style="color:#e2e8f0;margin-top:6px">{executive_summary or "No summary provided."}</div>
</div>

<!-- Top findings -->
<div class="label">Key Findings</div>
{cards}

<!-- Recommendations -->
<div class="label" style="margin-top:12px">Recommendations</div>
<div class="card">
    <ul style="color:#e2e8f0">{recs or "<li>No specific recommendations</li>"}</ul>
</div>
</body>
</html>"""


def handle_rpc(method, params, req_id):
    global _last_report
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}, "resources": {}},
            "serverInfo": {"name": "report-mcp-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}

        if name == "render-executive-summary":
            target = args.get("target", "")
            findings = args.get("findings", [])
            risk_score = args.get("risk_score", 0)
            executive_summary = args.get("executive_summary", "")
            recommendations = args.get("recommendations", [])
            result = render_executive_summary(target, findings, risk_score, executive_summary, recommendations)
            _last_report = result
            return {"content": [{"type": "text", "text": json.dumps(result, indent=2)}], "isError": False}, req_id

        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "report://executive":
            html = executive_report_html(_last_report)
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