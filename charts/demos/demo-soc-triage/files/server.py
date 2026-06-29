#!/usr/bin/env python3
"""
soc-apps-server: MCP HTTP transport server for the SOC Triage demo.

Exposes three tools with _meta.ui.resourceUri so the agent-orc UI renders
a sandboxed HTML panel below each tool result:

  cmdb-user-lookup(user_id)       → soc://cmdb        — personnel directory cards
  cmdb-ip-lookup(ip_address)      → soc://threat-intel — IP classification panel
  render-incident-report(...)     → soc://incident    — styled incident report card

The render-incident-report tool stores the last submitted report in memory and
serves it as HTML on resources/read.  All other HTML is generated fresh per request.
"""
import json, os, time
from http.server import BaseHTTPRequestHandler, HTTPServer
from datetime import datetime, timezone

PORT = int(os.environ.get("PORT", "8080"))

# ── CMDB data (mirrors the original sidecar CMDB) ────────────────────────────

USERS = {
    "john.doe@acme.corp": {
        "name": "John Doe", "title": "Sales Engineer", "dept": "Revenue", "level": "L3",
        "privileged_access": [], "known_ip_ranges": ["10.50.0.0/16", "192.168.1.0/24"],
        "account_status": "active", "mfa_enabled": True,
        "note": "Standard user. No privileged access. Works remotely from home (192.168.1.0/24).",
    },
    "sarah.chen@acme.corp": {
        "name": "Sarah Chen", "title": "Sr. DevOps Engineer", "dept": "Platform Engineering", "level": "L5",
        "privileged_access": ["prod-k8s-admin", "aws-iam-admin", "vault-operator"],
        "known_ip_ranges": ["10.10.0.0/16", "192.168.10.0/24"],
        "account_status": "active", "mfa_enabled": True,
        "note": "High-privilege user. Routinely accesses prod systems. Always on corp VPN.",
    },
    "admin-svc@acme.corp": {
        "name": "admin-svc", "title": "CI/CD Service Account", "dept": "Platform Engineering", "level": "SVC",
        "privileged_access": ["ci-pipeline-deployer", "ecr-push"],
        "known_ip_ranges": ["10.0.0.0/8"],
        "account_status": "active", "mfa_enabled": False,
        "note": "SERVICE ACCOUNT — interactive login is always anomalous. Must only authenticate from CI runners on 10.0.x.x.",
    },
    "carlos.martinez@acme.corp": {
        "name": "Carlos Martinez", "title": "Security Analyst", "dept": "InfoSec", "level": "L4",
        "privileged_access": ["splunk-admin", "crowdstrike-admin", "vault-read-all"],
        "known_ip_ranges": ["10.20.0.0/16", "192.168.20.0/24"],
        "account_status": "active", "mfa_enabled": True,
        "note": "SOC analyst. Read-only access to security tooling. Should not perform bulk exports.",
    },
}
# Short aliases
USERS["jdoe"] = USERS["john.doe@acme.corp"]
USERS["schen"] = USERS["sarah.chen@acme.corp"]
USERS["cmartinez"] = USERS["carlos.martinez@acme.corp"]

KNOWN_IPS = {
    "198.51.100.42": {"classification": "TOR_EXIT_NODE", "country": "NL", "org": "Tor Project",
                      "threat_intel": ["emergingthreats", "abuseipdb"],
                      "note": "Known Tor exit node — all traffic is anonymized."},
    "198.51.100.17": {"classification": "TOR_EXIT_NODE", "country": "DE", "org": "Tor Project",
                      "threat_intel": ["emergingthreats"],
                      "note": "Known Tor exit node."},
    "203.0.113.5":   {"classification": "THREAT_INTEL_FLAGGED", "country": "CN", "org": "Unknown Hosting Ltd",
                      "threat_intel": ["spamhaus_css", "abuseipdb"],
                      "note": "Flagged for C2 activity in the past 30 days."},
    "203.0.113.99":  {"classification": "THREAT_INTEL_FLAGGED", "country": "RU", "org": "Bulletproof Hosting LLC",
                      "threat_intel": ["emergingthreats_c2", "spamhaus_drop"],
                      "note": "CRITICAL — active C2 infrastructure."},
}

def classify_ip(ip):
    if ip in KNOWN_IPS:
        return KNOWN_IPS[ip]
    o = ip.split(".")
    if o[0] == "10":
        return {"classification": "CORPORATE_INTERNAL", "country": "US", "org": "Acme Corp",
                "threat_intel": [], "note": "Corporate internal network."}
    if o[0] == "192" and o[1] == "168":
        return {"classification": "CORPORATE_VPN", "country": "US", "org": "Acme Corp VPN",
                "threat_intel": [], "note": "Corporate VPN egress."}
    if o[0] in ("35", "52", "54", "34", "18"):
        return {"classification": "AWS_CLOUD_HOSTING", "country": "US", "org": "Amazon Web Services",
                "threat_intel": [], "note": "AWS IP — could be corporate workload or third party."}
    return {"classification": "UNKNOWN_EXTERNAL", "country": "UNKNOWN", "org": "UNKNOWN",
            "threat_intel": [], "note": "Unrecognized external IP — manual investigation required."}

# ── In-memory state ───────────────────────────────────────────────────────────

_last_report = None

# ── Tool definitions ──────────────────────────────────────────────────────────

TOOLS = [
    {
        "name": "cmdb-user-lookup",
        "description": (
            "Look up an employee or service account in the corporate CMDB. "
            "Returns role, department, privilege level, known IP ranges, and account notes."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "user_id": {"type": "string", "description": "Email address or username (e.g. john.doe@acme.corp or jdoe)"},
            },
            "required": ["user_id"],
        },
        "_meta": {"ui": {"resourceUri": "soc://cmdb"}},
    },
    {
        "name": "cmdb-ip-lookup",
        "description": (
            "Classify an IP address against the corporate IP registry and threat intelligence feeds. "
            "Returns classification, origin org, country, and threat intel flags."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "ip_address": {"type": "string", "description": "IPv4 address to classify"},
            },
            "required": ["ip_address"],
        },
        "_meta": {"ui": {"resourceUri": "soc://threat-intel"}},
    },
    {
        "name": "render-incident-report",
        "description": (
            "Submit a completed SOC incident report for visual rendering in the UI. "
            "Call this exactly once at the end of triage with the structured findings. "
            "The UI will display a styled incident card alongside your text report."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "risk_level": {
                    "type": "string",
                    "enum": ["LOW", "MEDIUM", "HIGH", "CRITICAL"],
                    "description": "Overall risk level from the risk assessor.",
                },
                "confidence": {
                    "type": "string",
                    "enum": ["low", "medium", "high"],
                    "description": "Confidence level from the risk assessor.",
                },
                "alert_summary": {
                    "type": "string",
                    "description": "One-line summary of the security alert.",
                },
                "key_indicators": {
                    "type": "array",
                    "items": {"type": "string"},
                    "description": "Key indicators that drove the risk assessment (3-5 items).",
                },
                "recommended_action": {
                    "type": "string",
                    "description": "Recommended immediate response action.",
                },
                "decision": {
                    "type": "string",
                    "description": "Final decision: 'approved', 'auto-closed', or 'escalated — <reason>'.",
                },
                "runbook": {
                    "type": "string",
                    "description": "Title of the matched runbook from the SOC KB, if any.",
                },
            },
            "required": ["risk_level", "alert_summary", "recommended_action", "decision"],
        },
        "_meta": {"ui": {"resourceUri": "soc://incident"}},
    },
]

# ── HTML builders ─────────────────────────────────────────────────────────────

_CSS_BASE = (
    "*{box-sizing:border-box;margin:0;padding:0}"
    "body{background:#0f172a;color:#e2e8f0;"
    "font-family:'Inter',system-ui,-apple-system,sans-serif;"
    "font-size:13px;padding:16px}"
    "h1{font-size:14px;font-weight:700;color:#f1f5f9;letter-spacing:.02em}"
    ".ts{color:#64748b;font-size:11px;margin-bottom:14px;margin-top:2px}"
    ".label{color:#64748b;font-size:10px;text-transform:uppercase;letter-spacing:.06em;margin-bottom:5px}"
    ".card{background:#1e293b;border:1px solid #334155;border-radius:6px;padding:12px 14px;margin-bottom:8px}"
    ".tag{display:inline-block;padding:2px 7px;border-radius:3px;font-size:10px;font-weight:700;"
    "letter-spacing:.04em;text-transform:uppercase;margin-right:4px;margin-bottom:3px}"
)


def cmdb_directory_html():
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S UTC")
    cards = ""
    # De-duplicate the aliases
    seen = set()
    for uid, u in USERS.items():
        if "@" not in uid:
            continue
        if uid in seen:
            continue
        seen.add(uid)
        priv = u["privileged_access"]
        priv_html = (
            "".join(
                f"<span class='tag' style='background:rgba(251,146,60,.15);color:#fb923c'>{p}</span>"
                for p in priv
            )
            if priv
            else "<span style='color:#475569;font-style:italic'>none</span>"
        )
        mfa_color = "#4ade80" if u["mfa_enabled"] else "#f87171"
        mfa_label = "MFA ✓" if u["mfa_enabled"] else "MFA ✗"
        level_color = "#f87171" if u["level"] == "SVC" else "#94a3b8"
        cards += (
            f"<div class='card'>"
            f"<div style='display:flex;align-items:center;gap:10px;margin-bottom:8px'>"
            f"<div style='width:32px;height:32px;border-radius:50%;background:#1e3a5f;"
            f"display:flex;align-items:center;justify-content:center;"
            f"color:#60a5fa;font-weight:700;font-size:13px'>"
            f"{u['name'][0]}</div>"
            f"<div><div style='color:#f1f5f9;font-weight:600'>{u['name']}</div>"
            f"<div style='color:#64748b;font-size:11px'>{u['title']} · {u['dept']}</div></div>"
            f"<span style='margin-left:auto;color:{level_color};font-size:11px;font-weight:600'>{u['level']}</span>"
            f"</div>"
            f"<div style='margin-bottom:6px'><div class='label'>Access</div>{priv_html}</div>"
            f"<div style='display:flex;gap:16px;font-size:11px;color:#64748b'>"
            f"<span>IPs: {', '.join(u['known_ip_ranges'])}</span>"
            f"<span style='margin-left:auto;color:{mfa_color}'>{mfa_label}</span>"
            f"</div>"
            f"<div style='color:#475569;font-size:11px;margin-top:6px;font-style:italic'>{u['note']}</div>"
            f"</div>"
        )
    return (
        f"<!DOCTYPE html><html lang='en'><head><meta charset='utf-8'>"
        f"<meta name='viewport' content='width=device-width,initial-scale=1'>"
        f"<title>CMDB Directory</title>"
        f"<style>{_CSS_BASE}</style></head><body>"
        f"<h1>Acme Corp CMDB</h1>"
        f"<div class='ts'>Personnel Directory — {ts}</div>"
        f"{cards}"
        f"</body></html>"
    )


def threat_intel_html():
    ts = datetime.now(timezone.utc).strftime("%H:%M:%S UTC")
    _class_styles = {
        "TOR_EXIT_NODE":       ("#f87171", "rgba(239,68,68,.18)"),
        "THREAT_INTEL_FLAGGED":("#fb923c", "rgba(249,115,22,.18)"),
        "CORPORATE_INTERNAL":  ("#4ade80", "rgba(74,222,128,.15)"),
        "CORPORATE_VPN":       ("#60a5fa", "rgba(96,165,250,.15)"),
        "AWS_CLOUD_HOSTING":   ("#a78bfa", "rgba(167,139,250,.15)"),
        "UNKNOWN_EXTERNAL":    ("#94a3b8", "rgba(148,163,184,.12)"),
    }
    cards = ""
    for ip, info in KNOWN_IPS.items():
        tc, bg = _class_styles.get(info["classification"], ("#94a3b8", "rgba(148,163,184,.12)"))
        feeds = (
            "".join(
                f"<span class='tag' style='background:rgba(239,68,68,.12);color:#f87171'>{f}</span>"
                for f in info["threat_intel"]
            )
            if info["threat_intel"]
            else "<span style='color:#475569;font-style:italic'>none</span>"
        )
        cards += (
            f"<div class='card'>"
            f"<div style='display:flex;align-items:center;gap:10px;margin-bottom:6px'>"
            f"<code style='color:#60a5fa;font-size:13px;font-weight:600'>{ip}</code>"
            f"<span class='tag' style='background:{bg};color:{tc};border:1px solid {tc};margin-left:auto'>"
            f"{info['classification']}</span>"
            f"</div>"
            f"<div style='color:#94a3b8;font-size:11px;margin-bottom:6px'>"
            f"{info['org']} · {info['country']}</div>"
            f"<div><div class='label'>Threat Intel Feeds</div>{feeds}</div>"
            f"<div style='color:#475569;font-size:11px;margin-top:6px;font-style:italic'>{info['note']}</div>"
            f"</div>"
        )
    # Append legend
    legend = "".join(
        f"<div style='display:flex;align-items:center;gap:8px;margin-bottom:4px'>"
        f"<span style='width:8px;height:8px;border-radius:2px;background:{tc};display:inline-block'></span>"
        f"<span style='color:#64748b;font-size:11px'>{cls}</span></div>"
        for cls, (tc, _) in _class_styles.items()
    )
    return (
        f"<!DOCTYPE html><html lang='en'><head><meta charset='utf-8'>"
        f"<meta name='viewport' content='width=device-width,initial-scale=1'>"
        f"<title>Threat Intel</title>"
        f"<style>{_CSS_BASE}</style></head><body>"
        f"<h1>Threat Intel Registry</h1>"
        f"<div class='ts'>Known malicious IPs — {ts}</div>"
        f"{cards}"
        f"<div style='margin-top:12px;padding:10px 14px;background:#1e293b;"
        f"border:1px solid #334155;border-radius:6px'>"
        f"<div class='label' style='margin-bottom:8px'>Classification Legend</div>"
        f"{legend}</div>"
        f"</body></html>"
    )


def incident_report_html(report):
    level = report.get("risk_level", "UNKNOWN")
    summary = report.get("alert_summary", "")
    confidence = report.get("confidence", "—")
    indicators = report.get("key_indicators", [])
    action = report.get("recommended_action", "—")
    decision = report.get("decision", "—")
    runbook = report.get("runbook", "")
    ts = datetime.fromtimestamp(report.get("submitted_at", time.time()), tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

    _level_styles = {
        "CRITICAL": ("#f87171", "rgba(239,68,68,.18)"),
        "HIGH":     ("#fb923c", "rgba(249,115,22,.18)"),
        "MEDIUM":   ("#facc15", "rgba(234,179,8,.15)"),
        "LOW":      ("#4ade80", "rgba(74,222,128,.15)"),
    }
    tc, bg = _level_styles.get(level, ("#94a3b8", "rgba(148,163,184,.15)"))

    indicator_items = "".join(
        f"<li style='padding:3px 0;color:#cbd5e1'>{ind}</li>"
        for ind in indicators
    ) or "<li style='color:#64748b;font-style:italic'>No indicators recorded.</li>"

    runbook_section = (
        f"<div style='margin-top:12px;padding:10px 14px;background:#1e293b;"
        f"border-left:3px solid #475569;border-radius:0 4px 4px 0'>"
        f"<div class='label'>Matched Runbook</div>"
        f"<div style='color:#e2e8f0;margin-top:4px'>{runbook}</div></div>"
    ) if runbook else ""

    decision_color = (
        "#f87171" if "escalat" in decision.lower()
        else "#4ade80" if "approved" in decision.lower() or "auto-closed" in decision.lower()
        else "#94a3b8"
    )

    return (
        f"<!DOCTYPE html><html lang='en'><head><meta charset='utf-8'>"
        f"<meta name='viewport' content='width=device-width,initial-scale=1'>"
        f"<title>SOC Incident</title>"
        f"<style>{_CSS_BASE}"
        f"ul{{list-style:disc;padding-left:18px}}"
        f".section{{margin-top:12px}}"
        f"</style></head><body>"
        f"<h1>Security Incident Report</h1>"
        f"<div class='ts'>{ts}</div>"
        f"<span style='display:inline-block;padding:4px 14px;border-radius:4px;"
        f"font-size:12px;font-weight:700;letter-spacing:.06em;text-transform:uppercase;"
        f"color:{tc};background:{bg};border:1px solid {tc}'>{level}</span>"
        f"<span style='color:#94a3b8;font-size:11px;margin-left:8px'>confidence: {confidence}</span>"
        f"<div class='section'>"
        f"<div class='label'>Alert</div>"
        f"<div style='color:#e2e8f0;line-height:1.6'>{summary}</div>"
        f"</div>"
        f"<div class='section'>"
        f"<div class='label'>Key Indicators</div>"
        f"<ul style='margin-top:4px'>{indicator_items}</ul>"
        f"</div>"
        f"<div class='section'>"
        f"<div class='label'>Recommended Action</div>"
        f"<div style='background:#1e293b;border:1px solid #334155;border-radius:6px;"
        f"padding:10px 14px;color:#f1f5f9;margin-top:4px'>{action}</div>"
        f"</div>"
        f"{runbook_section}"
        f"<div style='margin-top:12px;padding:10px 14px;background:#1e293b;"
        f"border-radius:6px;display:flex;align-items:center;gap:10px'>"
        f"<span class='label' style='margin-bottom:0'>Decision</span>"
        f"<span style='color:{decision_color};font-weight:500'>{decision}</span>"
        f"</div>"
        f"</body></html>"
    )


def empty_incident_html():
    return (
        "<!DOCTYPE html><html lang='en'><head><meta charset='utf-8'>"
        f"<style>*{{box-sizing:border-box;margin:0;padding:0}}"
        f"body{{background:#0f172a;color:#475569;"
        f"font-family:'Inter',system-ui,-apple-system,sans-serif;"
        f"font-size:13px;padding:16px;display:flex;align-items:center;"
        f"justify-content:center;min-height:80px}}</style></head>"
        "<body><span>Awaiting incident report\u2026</span></body></html>"
    )

# ── MCP JSON-RPC handler ──────────────────────────────────────────────────────

def handle_rpc(method, params, req_id):
    global _last_report
    params = params or {}

    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}, "resources": {}},
            "serverInfo": {"name": "soc-apps-server", "version": "1.0.0"},
        }, req_id

    if method == "notifications/initialized":
        return None, None

    if method == "tools/list":
        return {"tools": TOOLS}, req_id

    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}

        if name == "cmdb-user-lookup":
            uid = args.get("user_id", "").lower().strip()
            rec = USERS.get(uid, {"error": f"User '{uid}' not found in CMDB",
                                   "note": "Unknown user — may be external, recently created, or a typo."})
            return {"content": [{"type": "text", "text": json.dumps(rec, indent=2)}], "isError": False}, req_id

        if name == "cmdb-ip-lookup":
            ip = args.get("ip_address", "").strip()
            return {"content": [{"type": "text", "text": json.dumps(classify_ip(ip), indent=2)}], "isError": False}, req_id

        if name == "render-incident-report":
            _last_report = {
                "risk_level":         args.get("risk_level", "UNKNOWN"),
                "confidence":         args.get("confidence", "—"),
                "alert_summary":      args.get("alert_summary", ""),
                "key_indicators":     args.get("key_indicators", []),
                "recommended_action": args.get("recommended_action", ""),
                "decision":           args.get("decision", ""),
                "runbook":            args.get("runbook", ""),
                "submitted_at":       time.time(),
            }
            return {"content": [{"type": "text", "text": "Incident report rendered."}], "isError": False}, req_id

        return {"content": [{"type": "text", "text": "unknown tool: " + name}], "isError": True}, req_id

    if method == "resources/read":
        uri = params.get("uri", "")
        if uri == "soc://cmdb":
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": cmdb_directory_html()}]}, req_id
        if uri == "soc://threat-intel":
            return {"contents": [{"uri": uri, "mimeType": "text/html", "text": threat_intel_html()}]}, req_id
        if uri == "soc://incident":
            html = incident_report_html(_last_report) if _last_report else empty_incident_html()
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


print("soc-apps-server listening on :%d" % PORT, flush=True)
HTTPServer(("", PORT), MCPHandler).serve_forever()
