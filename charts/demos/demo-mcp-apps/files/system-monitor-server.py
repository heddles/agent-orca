#!/usr/bin/env python3
"""
system-monitor-server: MCP HTTP transport server for the demo-mcp-apps demo.

Exposes two tools:
  get_system_info(node?)    -- returns static node configuration; app: system monitor dashboard
  get_system_stats(node?)   -- returns live CPU/memory/disk metrics; app: system monitor dashboard

All JSON-RPC 2.0 requests arrive as POST / with Content-Type: application/json.
Responses are returned synchronously (HTTP transport, not SSE).
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── Synthetic data generators ──────────────────────────────────────────────────

NODES = [
    {"name": "node-1", "role": "control-plane", "zone": "us-east-1a"},
    {"name": "node-2", "role": "worker",        "zone": "us-east-1b"},
    {"name": "node-3", "role": "worker",        "zone": "us-east-1c"},
    {"name": "node-4", "role": "worker",        "zone": "us-east-1a"},
]

NODE_SPECS = {
    "node-1": {"cpu_model": "Intel Xeon E5-2686 v4", "cores": 4,  "memory_gb": 16,  "disk_gb": 100},
    "node-2": {"cpu_model": "Intel Xeon E5-2686 v4", "cores": 8,  "memory_gb": 32,  "disk_gb": 200},
    "node-3": {"cpu_model": "Intel Xeon E5-2686 v4", "cores": 8,  "memory_gb": 32,  "disk_gb": 200},
    "node-4": {"cpu_model": "Intel Xeon E5-2686 v4", "cores": 16, "memory_gb": 64,  "disk_gb": 500},
}

# Deterministic-ish seed so data looks stable across tool calls in the same hour.
_seed = int(time.time()) // 3600


def _rng(name, offset=0):
    """Seeded pseudo-random float in [0,1] for a given node name."""
    h = hash(name + str(_seed + offset)) & 0xFFFFFF
    return h / 0xFFFFFF


def node_info(node_name):
    spec = NODE_SPECS[node_name]
    node = next(n for n in NODES if n["name"] == node_name)
    return {
        "hostname": node_name,
        "role": node["role"],
        "zone": node["zone"],
        "platform": "linux",
        "architecture": "x86_64",
        "kernel": "6.1.109-118.189.amzn2023.x86_64",
        "cpu_model": spec["cpu_model"],
        "cpu_cores": spec["cores"],
        "memory_total_gb": spec["memory_gb"],
        "disk_total_gb": spec["disk_gb"],
    }


def node_stats(node_name):
    spec = NODE_SPECS[node_name]
    r = _rng(node_name)

    # Inject high CPU on node-2 to make the demo interesting.
    if node_name == "node-2":
        cpu_pct = round(78 + r * 15, 1)
        mem_pct = round(72 + r * 10, 1)
    elif r > 0.85:
        cpu_pct = round(65 + r * 25, 1)
        mem_pct = round(60 + r * 20, 1)
    else:
        cpu_pct = round(15 + r * 40, 1)
        mem_pct = round(30 + r * 35, 1)

    disk_pct = round(20 + _rng(node_name, 1) * 50, 1)
    uptime_days = int(12 + _rng(node_name, 2) * 90)

    mem_used = round(spec["memory_gb"] * mem_pct / 100, 1)
    disk_used = round(spec["disk_gb"] * disk_pct / 100, 1)

    return {
        "hostname": node_name,
        "cpu_usage_pct": cpu_pct,
        "cpu_cores": spec["cores"],
        "memory_used_gb": mem_used,
        "memory_total_gb": spec["memory_gb"],
        "memory_usage_pct": mem_pct,
        "disk_used_gb": disk_used,
        "disk_total_gb": spec["disk_gb"],
        "disk_usage_pct": disk_pct,
        "uptime_days": uptime_days,
    }


def cpu_history(node_name, points=20):
    """Generate realistic-looking CPU history data for a node."""
    base = node_stats(node_name)["cpu_usage_pct"]
    data = []
    for i in range(points):
        noise = (_rng(node_name, i + 10) - 0.5) * 20
        spike = 30 if (_rng(node_name, i + 200) > 0.93) else 0
        data.append(max(1, min(100, round(base + noise + spike, 1))))
    return data


def get_info_summary(node_filter=None):
    infos = []
    for n in NODES:
        if node_filter and n["name"] != node_filter:
            continue
        infos.append(node_info(n["name"]))
    if not infos:
        return "No node found matching '%s'." % node_filter
    lines = ["Cluster Node Information — %d node(s):" % len(infos), ""]
    for info in infos:
        lines.append("  %s (%s, %s)" % (info["hostname"], info["role"], info["zone"]))
        lines.append("    Platform: %s/%s  Kernel: %s" % (info["platform"], info["architecture"], info["kernel"]))
        lines.append("    CPU: %s (%d cores)  Memory: %dGB  Disk: %dGB" % (
            info["cpu_model"], info["cpu_cores"], info["memory_total_gb"], info["disk_total_gb"]))
        lines.append("")
    return "\n".join(lines)


def get_stats_summary(node_filter=None):
    stats = []
    for n in NODES:
        if node_filter and n["name"] != node_filter:
            continue
        stats.append(node_stats(n["name"]))
    if not stats:
        return "No node found matching '%s'." % node_filter

    ts = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    avg_cpu = round(sum(s["cpu_usage_pct"] for s in stats) / len(stats), 1)
    total_mem_used = round(sum(s["memory_used_gb"] for s in stats), 1)
    total_mem = sum(s["memory_total_gb"] for s in stats)

    lines = [
        "System Resource Usage — %s" % ts,
        "Nodes: %d   Avg CPU: %s%%   Memory: %s/%sGB" % (len(stats), avg_cpu, total_mem_used, total_mem),
        "",
    ]
    for s in stats:
        cpu_icon = "\u26a0" if s["cpu_usage_pct"] > 75 else "\u2713"
        lines.append("  %s %-12s cpu=%s%%  mem=%s/%sGB (%s%%)  disk=%s/%sGB (%s%%)  up=%dd" % (
            cpu_icon, s["hostname"], s["cpu_usage_pct"],
            s["memory_used_gb"], s["memory_total_gb"], s["memory_usage_pct"],
            s["disk_used_gb"], s["disk_total_gb"], s["disk_usage_pct"],
            s["uptime_days"]))
    return "\n".join(lines)


# ── HTML app ──────────────────────────────────────────────────────────────────

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


def build_progress_bar(pct, width=120, color="#3b82f6"):
    bar_color = "#22c55e" if pct < 60 else "#f59e0b" if pct < 80 else "#f87171"
    if color != "#3b82f6":
        bar_color = color
    fill = min(pct, 100)
    return (
        "<div style='display:inline-block;width:{w}px;height:8px;background:#1e293b;"
        "border-radius:4px;overflow:hidden;vertical-align:middle;margin-right:6px'>"
        "<div style='width:{f}%;height:100%;background:{c};border-radius:4px'></div>"
        "</div>"
    ).format(w=width, f=fill, c=bar_color)


def monitor_dashboard_html():
    all_stats = [node_stats(n["name"]) for n in NODES]
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S UTC")

    avg_cpu = round(sum(s["cpu_usage_pct"] for s in all_stats) / len(all_stats), 1)
    total_mem_used = round(sum(s["memory_used_gb"] for s in all_stats), 1)
    total_mem = sum(s["memory_total_gb"] for s in all_stats)
    total_disk_used = round(sum(s["disk_used_gb"] for s in all_stats), 1)
    total_disk = sum(s["disk_total_gb"] for s in all_stats)
    high_cpu_count = sum(1 for s in all_stats if s["cpu_usage_pct"] > 75)

    rows = ""
    for s in all_stats:
        node = next(n for n in NODES if n["name"] == s["hostname"])
        cpu_color = "#22c55e" if s["cpu_usage_pct"] < 60 else "#f59e0b" if s["cpu_usage_pct"] < 80 else "#f87171"
        mem_color = "#22c55e" if s["memory_usage_pct"] < 60 else "#f59e0b" if s["memory_usage_pct"] < 80 else "#f87171"
        disk_color = "#22c55e" if s["disk_usage_pct"] < 60 else "#f59e0b" if s["disk_usage_pct"] < 80 else "#f87171"
        spark = build_sparkline_svg(cpu_history(s["hostname"]), color=cpu_color)

        cpu_bar = build_progress_bar(s["cpu_usage_pct"], 80, cpu_color)
        mem_bar = build_progress_bar(s["memory_usage_pct"], 80, mem_color)
        disk_bar = build_progress_bar(s["disk_usage_pct"], 80, disk_color)
        rows += (
            "<tr data-node='{hostname}'>"
            "<td style='padding:8px 12px;white-space:nowrap'>"
            "<span style='display:inline-block;width:8px;height:8px;border-radius:50%;"
            "background:{cpu_color};margin-right:8px;vertical-align:middle'></span>"
            "<span style='color:#e2e8f0;font-weight:500'>{hostname}</span>"
            "<span style='color:#475569;font-size:10px;margin-left:6px'>{role}</span></td>"
            "<td style='padding:8px 12px;color:#94a3b8;font-size:11px'>{zone}</td>"
            "<td style='padding:8px 12px;text-align:right'>"
            "{cpu_bar}<span style='color:{cpu_color};font-size:12px;font-weight:600'>{cpu_pct}%</span></td>"
            "<td style='padding:8px 12px;text-align:right'>{spark}</td>"
            "<td style='padding:8px 12px;text-align:right'>"
            "{mem_bar}<span style='color:{mem_color};font-size:12px'>{mem_used}/{mem_total}GB</span></td>"
            "<td style='padding:8px 12px;text-align:right'>"
            "{disk_bar}<span style='color:{disk_color};font-size:12px'>{disk_used}/{disk_total}GB</span></td>"
            "<td style='padding:8px 12px;color:#64748b;font-size:12px;text-align:right'>{uptime}d</td>"
            "</tr>"
        ).format(
            hostname=s["hostname"], role=node["role"], zone=node["zone"],
            cpu_color=cpu_color, cpu_bar=cpu_bar, cpu_pct=s["cpu_usage_pct"], spark=spark,
            mem_color=mem_color, mem_bar=mem_bar, mem_used=s["memory_used_gb"], mem_total=s["memory_total_gb"],
            disk_color=disk_color, disk_bar=disk_bar, disk_used=s["disk_used_gb"], disk_total=s["disk_total_gb"],
            uptime=s["uptime_days"],
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
        "thead th:not(:first-child):not(:nth-child(2)){text-align:right}"
        "tbody tr{border-bottom:1px solid #1e293b;transition:opacity .15s}"
        "tbody tr:last-child{border-bottom:none}"
        "tbody tr.highlighted{background:rgba(59,130,246,.08);outline:1px solid rgba(59,130,246,.3)}"
    )

    avg_cpu_color = "#22c55e" if avg_cpu < 60 else "#f59e0b" if avg_cpu < 80 else "#f87171"
    mem_total_pct = round(total_mem_used / total_mem * 100, 1) if total_mem else 0
    mem_total_color = "#22c55e" if mem_total_pct < 60 else "#f59e0b" if mem_total_pct < 80 else "#f87171"

    js = (
        "window.addEventListener('message',function(e){"
        "if(e.source!==window.parent)return;"
        "var d=e.data;if(!d||d.type!=='mcp-app-result')return;"
        "var node=d.args&&d.args.node;"
        "var rows=document.querySelectorAll('tbody tr');"
        "rows.forEach(function(r){"
        "if(!node){r.style.opacity='1';r.classList.remove('highlighted');}"
        "else if(r.dataset.node===node){r.style.opacity='1';r.classList.add('highlighted');}"
        "else{r.style.opacity='0.3';r.classList.remove('highlighted');}"
        "});"
        "var sub=document.querySelector('.sub');"
        "if(sub)sub.textContent=node?'Filtered: '+node:'Updated %s';"
        "});"
    ) % ts

    return (
        "<!DOCTYPE html><html lang='en'><head>"
        "<meta charset='utf-8'>"
        "<meta name='viewport' content='width=device-width,initial-scale=1'>"
        "<title>System Monitor</title>"
        "<style>%s</style></head><body>"
        "<h1>System Monitor</h1>"
        "<div class='sub'>Updated %s</div>"
        "<div class='stats'>"
        "<div class='stat'><div class='stat-val' style='color:%s'>%s%%</div><div class='stat-lbl'>Avg CPU</div></div>"
        "<div class='stat'><div class='stat-val' style='color:%s'>%s/%sGB</div><div class='stat-lbl'>Memory</div></div>"
        "<div class='stat'><div class='stat-val' style='color:#94a3b8'>%s/%sGB</div><div class='stat-lbl'>Disk</div></div>"
        "<div class='stat'><div class='stat-val' style='color:%s'>%d</div><div class='stat-lbl'>High CPU</div></div>"
        "</div>"
        "<table><thead><tr>"
        "<th>Node</th><th>Zone</th><th>CPU</th><th>CPU Trend</th><th>Memory</th><th>Disk</th><th>Uptime</th>"
        "</tr></thead><tbody>%s</tbody></table>"
        "<script>%s</script>"
        "</body></html>"
    ) % (
        css, ts,
        avg_cpu_color, avg_cpu,
        mem_total_color, total_mem_used, total_mem,
        total_disk_used, total_disk,
        "#f87171" if high_cpu_count > 0 else "#22c55e", high_cpu_count,
        rows, js,
    )


# ── MCP JSON-RPC handler ───────────────────────────────────────────────────────

TOOLS = [
    {
        "name": "get-system-info",
        "description": (
            "Return static configuration for all cluster nodes (or a specific node). "
            "Shows hostname, role, zone, CPU model, core count, memory, and disk capacity."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "node": {
                    "type": "string",
                    "description": "Optional. Filter to a specific node name (e.g. 'node-2'). Omit for all nodes.",
                }
            },
            "required": [],
        },
        "_meta": {"ui": {"resourceUri": "system://monitor"}},
    },
    {
        "name": "get-system-stats",
        "description": (
            "Return current resource usage for all cluster nodes (or a specific node). "
            "Shows CPU usage, memory used/total, disk used/total, and uptime."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "node": {
                    "type": "string",
                    "description": "Optional. Filter to a specific node name (e.g. 'node-2'). Omit for all nodes.",
                }
            },
            "required": [],
        },
        "_meta": {"ui": {"resourceUri": "system://monitor"}},
    },
]


def handle_rpc(method, params, req_id):
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}, "resources": {}},
            "serverInfo": {"name": "system-monitor-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None  # notification — no response

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}
        if name == "get-system-info":
            text = get_info_summary(args.get("node"))
            return {"content": [{"type": "text", "text": text}], "isError": False}, req_id
        if name == "get-system-stats":
            text = get_stats_summary(args.get("node"))
            return {"content": [{"type": "text", "text": text}], "isError": False}, req_id
        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "system://monitor":
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": monitor_dashboard_html()}]}, req_id
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


print("system-monitor-server listening on :%d" % PORT, flush=True)
HTTPServer(("", PORT), MCPHandler).serve_forever()
