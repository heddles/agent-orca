#!/usr/bin/env python3
"""
health-mcp-server: MCP HTTP transport server for the demo-mcp-apps demo.

Exposes two tools:
  check_service_health(service?)  -- returns health summary; app: service status dashboard
  list_incidents(severity?)       -- returns open incidents; app: incident timeline

All JSON-RPC 2.0 requests arrive as POST / with Content-Type: application/json.
Responses are returned synchronously (HTTP transport, not SSE).
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── Synthetic data generators ──────────────────────────────────────────────────

SERVICES = [
    {"name": "api-gateway",          "tier": "edge",    "owner": "platform"},
    {"name": "auth-service",         "tier": "core",    "owner": "security"},
    {"name": "payment-service",      "tier": "core",    "owner": "fintech"},
    {"name": "user-service",         "tier": "core",    "owner": "platform"},
    {"name": "notification-service", "tier": "async",   "owner": "platform"},
    {"name": "search-service",       "tier": "read",    "owner": "discovery"},
]

# Deterministic-ish seed so data looks stable across tool calls in the same hour.
_seed = int(time.time()) // 3600

def _rng(name, offset=0):
    """Seeded pseudo-random float in [0,1] for a given service name."""
    h = hash(name + str(_seed + offset)) & 0xFFFFFF
    return h / 0xFFFFFF

def service_metrics(svc_name):
    r = _rng(svc_name)
    # Inject a degraded state for payment-service to make the demo interesting.
    if svc_name == "payment-service":
        status = "degraded"
        uptime = round(98.1 + r * 0.5, 2)
        rps = round(140 + r * 60)
        p99_ms = round(480 + r * 200)
        error_rate = round(2.1 + r * 1.5, 2)
    elif r > 0.92:
        status = "degraded"
        uptime = round(97.5 + r * 1.5, 2)
        rps = round(50 + r * 80)
        p99_ms = round(350 + r * 300)
        error_rate = round(1.5 + r * 2.0, 2)
    else:
        status = "healthy"
        uptime = round(99.5 + r * 0.49, 2)
        rps = round(100 + r * 400)
        p99_ms = round(20 + r * 80)
        error_rate = round(r * 0.3, 2)
    return {
        "name": svc_name,
        "status": status,
        "uptime_pct": uptime,
        "requests_per_sec": rps,
        "p99_latency_ms": p99_ms,
        "error_rate_pct": error_rate,
    }

def sparkline_data(svc_name, points=20):
    """Generate realistic-looking p99 sparkline data for a service."""
    base = service_metrics(svc_name)["p99_latency_ms"]
    data = []
    for i in range(points):
        noise = (_rng(svc_name, i) - 0.5) * base * 0.4
        spike = base * 3 if (_rng(svc_name, i + 100) > 0.92) else 0
        data.append(max(5, int(base + noise + spike)))
    return data

INCIDENT_POOL = [
    {"id": "INC-4821", "service": "payment-service",      "severity": "P1", "title": "Elevated error rate on /v2/charge endpoint",      "started": "14m ago",   "status": "investigating"},
    {"id": "INC-4819", "service": "auth-service",         "severity": "P2", "title": "JWT validation latency spike (p99 > 400ms)",       "started": "1h 2m ago", "status": "monitoring"},
    {"id": "INC-4815", "service": "search-service",       "severity": "P3", "title": "Index rebuild causing cache eviction pressure",    "started": "3h ago",    "status": "in-progress"},
    {"id": "INC-4801", "service": "notification-service", "severity": "P3", "title": "Email delivery queue depth above threshold (12k)", "started": "6h ago",    "status": "in-progress"},
]

def get_health_summary(service_filter=None):
    metrics = []
    for svc in SERVICES:
        if service_filter and svc["name"] != service_filter:
            continue
        metrics.append(service_metrics(svc["name"]))
    healthy = sum(1 for m in metrics if m["status"] == "healthy")
    degraded = sum(1 for m in metrics if m["status"] == "degraded")
    lines = [
        "Service Health Summary — " + datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "Healthy: %d/%d   Degraded: %d/%d" % (healthy, len(metrics), degraded, len(metrics)),
        "",
    ]
    for m in metrics:
        icon = "\u2713" if m["status"] == "healthy" else "\u26a0"
        lines.append("  %s %-24s uptime=%s%%  rps=%s  p99=%sms  errors=%s%%" % (
            icon, m["name"], m["uptime_pct"], m["requests_per_sec"],
            m["p99_latency_ms"], m["error_rate_pct"]))
    return "\n".join(lines)

def get_incidents(severity_filter=None):
    incidents = INCIDENT_POOL
    if severity_filter:
        incidents = [i for i in incidents if i["severity"] == severity_filter.upper()]
    if not incidents:
        return "No open incidents" + (" at severity " + severity_filter.upper() if severity_filter else "") + "."
    lines = ["Open Incidents (%d total):" % len(incidents), ""]
    for inc in incidents:
        lines.append("  [%s] %s  %s  (%s)  %s" % (
            inc["severity"], inc["id"], inc["service"], inc["started"], inc["status"]))
        lines.append("       " + inc["title"])
    return "\n".join(lines)

# ── HTML apps ──────────────────────────────────────────────────────────────────

def build_sparkline_svg(data, width=80, height=24, color="#22c55e"):
    if not data:
        return ""
    mn, mx = min(data), max(data)
    rng = mx - mn or 1
    pts = []
    for i, v in enumerate(data):
        x = round(i * width / (len(data) - 1), 1)
        y = round(height - (v - mn) / rng * (height - 2) - 1, 1)
        pts.append("%s,%s" % (x, y))
    path = "M" + " L".join(pts)
    return (
        '<svg width="%d" height="%d" viewBox="0 0 %d %d" style="overflow:visible">'
        '<path d="%s" fill="none" stroke="%s" stroke-width="1.5" stroke-linejoin="round"/>'
        '</svg>' % (width, height, width, height, path, color)
    )

def health_dashboard_html():
    all_metrics = [service_metrics(s["name"]) for s in SERVICES]
    healthy_count = sum(1 for m in all_metrics if m["status"] == "healthy")
    degraded_count = sum(1 for m in all_metrics if m["status"] == "degraded")
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S UTC")
    open_incs = len(INCIDENT_POOL)
    p1_count = sum(1 for i in INCIDENT_POOL if i["severity"] == "P1")

    rows = ""
    for m in all_metrics:
        ok = m["status"] == "healthy"
        dot_color = "#22c55e" if ok else "#f59e0b"
        label_color = "#22c55e" if ok else "#f59e0b"
        spark_color = "#22c55e" if ok else "#f59e0b"
        spark = build_sparkline_svg(sparkline_data(m["name"]), color=spark_color)
        err_color = "#f87171" if m["error_rate_pct"] > 1 else "#94a3b8"
        rows += (
            "<tr data-service='%s'>"
            "<td style='padding:8px 12px;white-space:nowrap'>"
            "<span style='display:inline-block;width:8px;height:8px;border-radius:50%;"
            "background:%s;margin-right:8px;vertical-align:middle'></span>"
            "<span style='color:#e2e8f0;font-weight:500'>%s</span></td>"
            "<td style='padding:8px 12px;color:%s;font-weight:600;font-size:11px;"
            "text-transform:uppercase'>%s</td>"
            "<td style='padding:8px 12px;color:#94a3b8;font-size:12px;text-align:right'>%s%%</td>"
            "<td style='padding:8px 12px;color:#94a3b8;font-size:12px;text-align:right'>%s</td>"
            "<td style='padding:8px 12px;text-align:right'>%s</td>"
            "<td style='padding:8px 12px;color:#94a3b8;font-size:12px;text-align:right'>%sms</td>"
            "<td style='padding:8px 12px;color:%s;font-size:12px;text-align:right'>%s%%</td>"
            "</tr>"
        ) % (
            m["name"],
            dot_color, m["name"],
            label_color, m["status"],
            m["uptime_pct"],
            m["requests_per_sec"],
            spark,
            m["p99_latency_ms"],
            err_color, m["error_rate_pct"],
        )

    css = (
        "*{box-sizing:border-box;margin:0;padding:0}"
        "body{background:#0f172a;color:#e2e8f0;font-family:'Inter',system-ui,-apple-system,sans-serif;"
        "font-size:13px;padding:16px}"
        "h1{font-size:14px;font-weight:700;color:#f1f5f9;letter-spacing:.02em;margin-bottom:4px}"
        ".sub{color:#64748b;font-size:11px;margin-bottom:14px}"
        ".stats{display:flex;gap:12px;margin-bottom:14px}"
        ".stat{background:#1e293b;border:1px solid #334155;border-radius:6px;padding:10px 14px;flex:1}"
        ".stat-val{font-size:22px;font-weight:700;line-height:1}"
        ".stat-lbl{color:#64748b;font-size:10px;text-transform:uppercase;letter-spacing:.06em;margin-top:3px}"
        "table{width:100%;border-collapse:collapse}"
        "thead th{padding:6px 12px;text-align:left;font-size:10px;font-weight:600;color:#475569;"
        "text-transform:uppercase;letter-spacing:.06em;border-bottom:1px solid #1e293b}"
        "thead th:not(:first-child){text-align:right}"
        "tbody tr{border-bottom:1px solid #1e293b;transition:opacity .15s}"
        "tbody tr:last-child{border-bottom:none}"
        "tbody tr.highlighted{background:rgba(16,185,129,.08);outline:1px solid rgba(16,185,129,.3)}"
    )

    # postMessage listener: parent injects {type:"mcp-app-result", tool, args:{service?}} after each tool call.
    # Highlight the queried service row and dim the rest so the iframe reflects what the LLM asked about.
    js = (
        "window.addEventListener('message',function(e){"
        "if(e.source!==window.parent)return;"
        "var d=e.data;if(!d||d.type!=='mcp-app-result')return;"
        "var svc=d.args&&d.args.service;"
        "var rows=document.querySelectorAll('tbody tr');"
        "rows.forEach(function(r){"
        "if(!svc){r.style.opacity='1';r.classList.remove('highlighted');}"
        "else if(r.dataset.service===svc){r.style.opacity='1';r.classList.add('highlighted');}"
        "else{r.style.opacity='0.3';r.classList.remove('highlighted');}"
        "});"
        "var sub=document.querySelector('.sub');"
        "if(sub)sub.textContent=svc?'Filtered: '+svc:'Updated %s';"
        "});"
    ) % ts

    return (
        "<!DOCTYPE html><html lang='en'><head>"
        "<meta charset='utf-8'>"
        "<meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>Service Health</title>"
        "<style>%s</style></head><body>"
        "<h1>Service Health Dashboard</h1>"
        "<div class='sub'>Updated %s</div>"
        "<div class='stats'>"
        "<div class='stat'><div class='stat-val' style='color:#22c55e'>%d</div><div class='stat-lbl'>Healthy</div></div>"
        "<div class='stat'><div class='stat-val' style='color:#f59e0b'>%d</div><div class='stat-lbl'>Degraded</div></div>"
        "<div class='stat'><div class='stat-val' style='color:#f87171'>%d</div><div class='stat-lbl'>P1 Incidents</div></div>"
        "<div class='stat'><div class='stat-val' style='color:#94a3b8'>%d</div><div class='stat-lbl'>Open Incidents</div></div>"
        "</div>"
        "<table><thead><tr>"
        "<th>Service</th><th>Status</th><th>Uptime</th><th>Req/s</th><th>p99 Trend</th><th>p99</th><th>Errors</th>"
        "</tr></thead><tbody>%s</tbody></table>"
        "<script>%s</script>"
        "</body></html>"
    ) % (css, ts, healthy_count, degraded_count, p1_count, open_incs, rows, js)

def incidents_dashboard_html():
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S UTC")
    sev_colors = {
        "P1": ("#f87171", "rgba(239,68,68,.15)"),
        "P2": ("#fb923c", "rgba(249,115,22,.15)"),
        "P3": ("#facc15", "rgba(234,179,8,.12)"),
        "P4": ("#94a3b8", "rgba(100,116,139,.12)"),
    }
    status_colors = {
        "investigating": "#f87171",
        "monitoring": "#fb923c",
        "in-progress": "#facc15",
        "resolved": "#22c55e",
    }

    cards = ""
    for inc in INCIDENT_POOL:
        tc, bg = sev_colors.get(inc["severity"], ("#94a3b8", "rgba(148,163,184,.1)"))
        sc = status_colors.get(inc["status"], "#94a3b8")
        cards += (
            "<div data-severity='%s' style='background:#1e293b;border:1px solid #334155;border-radius:6px;"
            "padding:12px;margin-bottom:8px;transition:opacity .15s'>"
            "<div style='display:flex;align-items:center;gap:8px;margin-bottom:6px'>"
            "<span style='background:%s;color:%s;padding:2px 8px;border-radius:3px;"
            "font-size:10px;font-weight:700'>%s</span>"
            "<span style='color:#64748b;font-size:11px;font-family:monospace'>%s</span>"
            "<span style='margin-left:auto;color:%s;font-size:10px;text-transform:uppercase;"
            "letter-spacing:.05em'>%s</span></div>"
            "<div style='color:#f1f5f9;font-weight:500;margin-bottom:4px'>%s</div>"
            "<div style='display:flex;gap:16px;color:#64748b;font-size:11px'>"
            "<span>&#128295; %s</span><span>&#128337; %s</span></div>"
            "</div>"
        ) % (inc["severity"], bg, tc, inc["severity"], inc["id"], sc, inc["status"],
             inc["title"], inc["service"], inc["started"])

    p_counts = {}
    for inc in INCIDENT_POOL:
        p_counts[inc["severity"]] = p_counts.get(inc["severity"], 0) + 1

    stat_html = ""
    for sev in ["P1", "P2", "P3", "P4"]:
        count = p_counts.get(sev, 0)
        tc, bg = sev_colors[sev]
        stat_html += (
            "<div class='stat'>"
            "<div class='stat-val' style='color:%s'>%d</div>"
            "<div class='stat-lbl'>%s Open</div></div>"
        ) % (tc, count, sev)

    css = (
        "*{box-sizing:border-box;margin:0;padding:0}"
        "body{background:#0f172a;color:#e2e8f0;font-family:'Inter',system-ui,-apple-system,sans-serif;"
        "font-size:13px;padding:16px}"
        "h1{font-size:14px;font-weight:700;color:#f1f5f9;letter-spacing:.02em;margin-bottom:4px}"
        ".sub{color:#64748b;font-size:11px;margin-bottom:14px}"
        ".stats{display:flex;gap:12px;margin-bottom:14px}"
        ".stat{background:#1e293b;border:1px solid #334155;border-radius:6px;padding:10px 14px;flex:1}"
        ".stat-val{font-size:22px;font-weight:700;line-height:1}"
        ".stat-lbl{color:#64748b;font-size:10px;text-transform:uppercase;letter-spacing:.06em;margin-top:3px}"
    )

    # postMessage listener: parent injects {type:"mcp-app-result", tool, args:{severity?}} after each tool call.
    # Highlight matching severity cards and dim the rest.
    js = (
        "window.addEventListener('message',function(e){"
        "if(e.source!==window.parent)return;"
        "var d=e.data;if(!d||d.type!=='mcp-app-result')return;"
        "var sev=d.args&&d.args.severity;"
        "var cards=document.querySelectorAll('[data-severity]');"
        "cards.forEach(function(c){"
        "c.style.opacity=(!sev||c.dataset.severity===sev)?'1':'0.3';"
        "});"
        "var sub=document.querySelector('.sub');"
        "if(sub&&sev){var n=Array.from(cards).filter(function(c){return c.dataset.severity===sev;}).length;"
        "sub.textContent='Filtered: '+sev+' \u2014 '+n+' open';}"
        "else if(sub){sub.textContent='Updated %s \u2014 %d open';}"
        "});"
    ) % (ts, len(INCIDENT_POOL))

    return (
        "<!DOCTYPE html><html lang='en'><head>"
        "<meta charset='utf-8'>"
        "<meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>Incidents</title>"
        "<style>%s</style></head><body>"
        "<h1>Open Incidents</h1>"
        "<div class='sub'>Updated %s &mdash; %d open</div>"
        "<div class='stats'>%s</div>%s"
        "<script>%s</script>"
        "</body></html>"
    ) % (css, ts, len(INCIDENT_POOL), stat_html, cards, js)

# ── MCP JSON-RPC handler ───────────────────────────────────────────────────────

TOOLS = [
    {
        "name": "check-service-health",
        "description": (
            "Return the current health status of all services (or a specific service). "
            "Shows uptime percentage, requests per second, p99 latency, and error rate."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "service": {
                    "type": "string",
                    "description": "Optional. Filter to a specific service name (e.g. 'payment-service'). Omit for all services.",
                }
            },
            "required": [],
        },
        "_meta": {"ui": {"resourceUri": "health://dashboard"}},
    },
    {
        "name": "list-incidents",
        "description": (
            "List all currently open incidents, optionally filtered by severity (P1-P4). "
            "Each incident includes its service, age, status, and description."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "severity": {
                    "type": "string",
                    "enum": ["P1", "P2", "P3", "P4"],
                    "description": "Optional. Filter by severity level. Omit to see all open incidents.",
                }
            },
            "required": [],
        },
        "_meta": {"ui": {"resourceUri": "health://incidents"}},
    },
]


def handle_rpc(method, params, req_id):
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}, "resources": {}},
            "serverInfo": {"name": "health-mcp-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None  # notification — no response

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}
        if name == "check-service-health":
            text = get_health_summary(args.get("service"))
            return {"content": [{"type": "text", "text": text}], "isError": False}, req_id
        if name == "list-incidents":
            text = get_incidents(args.get("severity"))
            return {"content": [{"type": "text", "text": text}], "isError": False}, req_id
        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "health://dashboard":
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": health_dashboard_html()}]}, req_id
        if uri == "health://incidents":
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": incidents_dashboard_html()}]}, req_id
        return {"error": {"code": -32002, "message": "Resource not found: " + uri}}, req_id

    return {"error": {"code": -32601, "message": "Method not found: " + method}}, req_id


class MCPHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):
        pass  # suppress access log noise

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

        method = req.get("method", "")
        params = req.get("params")
        req_id = req.get("id")

        result, resp_id = handle_rpc(method, params, req_id)
        if result is None and resp_id is None:
            # Notification — return 204
            self.send_response(204)
            self.end_headers()
            return

        if isinstance(result, dict) and "error" in result and len(result) == 1:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "error": result["error"]})
        else:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "result": result})

    def _respond(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


print("health-mcp-server listening on :%d" % PORT, flush=True)
HTTPServer(("", PORT), MCPHandler).serve_forever()
