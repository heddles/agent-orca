#!/usr/bin/env python3
"""
pwnbox-mcp-server: MCP (JSON-RPC over HTTP) server for the HTB pwnbox.

Runs INSIDE the privileged red-pwnbox pod, AFTER entrypoint.sh has brought up the
HTB OpenVPN tunnel on tun0. Exposes the pentest toolbelt as MCP tools so the
agent-orc model-router (in the *separate, restricted* red-commander agent pod)
discovers and calls them; the tools themselves execute here, over tun0, with the
real nmap/enum4linux-ng/impacket/nuclei binaries.

The agent never has the tunnel — only this pod does — so the LLM brain stays fully
restricted while the muscle runs privileged tools over the lab network.

JSON-RPC surface (mirrors charts/demos/demo-soc-triage/files/server.py):
  initialize            -> protocol + capabilities
  tools/list            -> the tool belt
  tools/call            -> dispatch to local subprocess
  notifications/initialized -> 204

TIMEOUT MODEL
  - model-router ToolExecutionTimeoutSec default is now 1h (3600s).
  - TOOL_TIMEOUT = 3600 is the per-tool ceiling (matches the router default); every
    tool's requested `timeout` arg is clamped into [1, TOOL_TIMEOUT].
  - DEFAULT_TOOL_TIMEOUT = 300s: the *default* requested timeout for individual
    scans so the LLM gets responsive results (one host/port-range per call); the LLM
    may raise it up to the 3600s ceiling for a long-running scan.
  - Output is truncated per-call (TOUT_PULL / TOUT_PUSH caps) to bound response
    tokens — the cap is about token budget, not averting a router-imposed abort.
"""
import json, os, re, shlex, ssl, shutil, subprocess, sys, time, inspect
from pathlib import Path
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from datetime import datetime, timezone
from urllib.parse import urlparse

PORT = int(os.environ.get("PORT", "8080"))

# ── Timeout model ──────────────────────────────────────────────────────────────
# Router default is now 1h. Ceiling here mirrors it so an LLM-requested long scan
# can run the full window; the default call stays responsive (300s).
TOOL_TIMEOUT = int(os.environ.get("TOOL_TIMEOUT", "3600"))          # hard ceiling (1h)
DEFAULT_TOOL_TIMEOUT = int(os.environ.get("DEFAULT_TOOL_TIMEOUT", "300"))  # default per-call

# Bound the *captured* output so a single tool result can't balloon the context.
TOUT_PULL = 12000   # light recon tools (scans/lists)
TOUT_PUSH = 16000   # heavier tools (exploit output, file reads)
TOUT_FETCH = 6000   # lookup-style tools (searchsploit/table output)

# ── Exploit sandbox ───────────────────────────────────────────────────────────
# fetch-exploit / run-exploit write ONLY inside EXPLOIT_DIR. The dir is (re)created
# empty by entrypoint.sh on every pod start so stale payloads never persist.
EXPLOIT_DIR = os.environ.get("EXPLOIT_DIR", "/tmp/exploits")
# Remote hosts fetch-exploit is allowed to curl. Empty (the default) disables remote
# fetch entirely — only local ExploitDB (searchsploit) is permitted. Operator opt-in:
#   EXPLOIT_FETCH_ALLOWLIST="raw.githubusercontent.com,gitlab.com"
ALLOWED_FETCH_HOSTS = {
    h for h in re.split(r"[,\s]+", os.environ.get("EXPLOIT_FETCH_ALLOWLIST", "")) if h
}


def _host_allowed(url):
    """True if `url`'s host is in the (case-insensitive) EXPLOIT_FETCH_ALLOWLIST."""
    if not ALLOWED_FETCH_HOSTS:
        return False
    try:
        host = urlparse(url).hostname or ""
    except Exception:
        return False
    return host.lower() in ALLOWED_FETCH_HOSTS


# ── Tool belt (declared here == declared in the MCPServer CR's `tools:` list) ────

TOOLS = [
    {
        "name": "probe-target",
        "description": (
            "Run an nmap service/version scan against an HTB lab target reachable via "
            "the tun0 tunnel. Use a lab IP/CIDR only. Returns open ports + service banners."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string", "description": "Lab IP or CIDR, e.g. 10.10.14.23"},
                "ports":  {"type": "string", "description": "nmap -p range, e.g. '1-1000' or '445,80'", "default": "1-1000"},
                "timeout":{"type": "integer", "description": f"Scan hard timeout in seconds (1-{TOOL_TIMEOUT}). Default {DEFAULT_TOOL_TIMEOUT}.", "default": DEFAULT_TOOL_TIMEOUT},
            },
            "required": ["target"],
        },
    },
    {
        "name": "enum-shares",
        "description": (
            "Enumerate SMB/users/shares on an HTB target via enum4linux-ng. Bounded by "
            "the model-router's tool timeout — for full enumeration run targeted and re-run per host."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string", "description": "Lab IP"},
                "timeout":{"type": "integer", "description": f"Hard timeout in seconds (1-{TOOL_TIMEOUT}). Default {DEFAULT_TOOL_TIMEOUT}.", "default": DEFAULT_TOOL_TIMEOUT},
            },
            "required": ["target"],
        },
    },
    {
        "name": "shell-exploit",
        "description": (
            "Run an nmap NSE script exploit against a target (e.g. 'vulners', 'smb-vuln-*', "
            "'ssl-enum'). Keep payloads targeted. For metasploit or custom payloads, use "
            "msfvenom-payload / metasploit-exploit / run-exploit."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "technique": {"type": "string", "description": "nmap NSE script name/category"},
                "target":    {"type": "string", "description": "Lab IP"},
                "payload":   {"type": "string", "description": "Optional extra args / payload"},
                "timeout":   {"type": "integer", "description": f"Hard timeout in seconds (1-{TOOL_TIMEOUT}). Default {DEFAULT_TOOL_TIMEOUT}.", "default": DEFAULT_TOOL_TIMEOUT},
            },
            "required": ["technique", "target"],
        },
    },
    {
        "name": "submit-findings",
        "description": (
            "Record a captured flag/finding. Call once per flag with the flag value + a short "
            "detail string. Writes to stdout (kubectl logs) and, if REDIS_ADDRESS is set, to "
            "the `arena-events` Redis stream."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "flag":    {"type": "string", "description": "The captured flag/value"},
                "details": {"type": "string", "description": "Short context: host, technique, path"},
            },
            "required": ["flag", "details"],
        },
    },
    # ── ENGAGEMENT STATE / FLAG HARVEST ─────────────────────────────────────────
    # These exist so the agent can (a) see "what has already been done / found"
    # without loading the multi-MB trajectory into context, and (b) search the
    # pwnbox's exfil/working dirs for exfiltrated flags without hand-rolling
    # grep/findstr recursions (the #1 source of context bloat in prior runs).
    {
        "name": "flag-harvest",
        "description": (
            "Search the pwnbox filesystem (typically /tmp exfiltration dirs) for files or "
            "binaries containing the engagement's flag pattern, and for flags already "
            "recorded in the arena-events Redis stream. Replaces hand-rolled "
            "findstr/Get-ChildItem/grep recursions that bloat context. Returns matches "
            "capped to a bounded set. The default `pattern` is a broad CTF regex "
            "(TAG{32-hex}); pass the confirmed engagement format to tighten it."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "target": {"type": "string",
                           "description": "Directory to search (e.g. /tmp, /tmp/exploits, /tmp/ftp_anon). Default /tmp.",
                           "default": "/tmp"},
                "pattern": {"type": "string",
                            "description": "Regex for the flag format. Default is a broad CTF pattern that matches HTB{}, PUPPET{}, etc. — override with the engagement's confirmed format (e.g. 'PUPPET\\\\{[a-f0-9]{32}\\\\}') when known to cut false positives.",
                            "default": "[A-Z0-9_-]+\\{[a-f0-9]{32}\\}"},
                "timeout": {"type": "integer",
                            "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}",
                            "default": DEFAULT_TOOL_TIMEOUT},
            },
            "required": [],
        },
    },
    {
        "name": "engagement-state",
        "description": (
            "Return a COMPACT engagement snapshot — tun0/VPN status, openvpn log tail, "
            "sliver beacon/session counts, exploit-workspace listing, and flags already "
            "recorded in the arena-events Redis stream. Use this instead of reading the "
            "multi-MB trajectory to answer 'what have we done and what's left?'."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "timeout": {"type": "integer",
                            "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}",
                            "default": DEFAULT_TOOL_TIMEOUT},
            },
            "required": [],
        },
    },
    # ── SESSION / TCP ──────────────────────────────────────────────────────────────
    # These bridge agent-orc's request/response tool model to persistent, interactive
    # tooling (Sliver TUI, raw C2 sockets). Output is returned as the tool-result
    # string (NOT files) — see docs/redis.md / entrypoint runbook note: "logging all
    # output to files will not work for kubernetes".
    {
        "name": "tcp-connect",
        "description": (
            "Open a raw TCP (or TLS) connection to host:port, optionally send a payload, "
            "read the response, and close. Use for C2 probes / custom protocols that have "
            "no HTTP wrapper. tun0 is NOT required (targets may be lab or external). "
            "Prefer over a persistent session only for one-shot request/response; for "
            "back-and-forth exchanges, tee a tmux pane and drive it with tmux-send."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "host":  {"type": "string", "description": "Target hostname or IP"},
                "port":  {"type": "integer", "description": "Target TCP port", "minimum": 1, "maximum": 65535},
                "data":  {"type": "string", "description": "(Optional) bytes to send immediately after connecting (e.g. an HTTP request or protocol greeting).", "default": ""},
                "timeout": {"type": "integer", "description": "Idle+total timeout in seconds (1-600). Default 30.", "default": 30},
                "ssl":   {"type": "boolean", "description": "Wrap the connection in TLS (--ssl). Default false.", "default": False},
            },
            "required": ["host", "port"],
        },
    },
    {
        "name": "tmux-new",
        "description": (
            "Create a detached, persistent interactive session (a shell by default) that "
            "survives across MCP tool calls. Use this to host a Sliver client, an ncat C2 "
            "relay, or any TUI/binary that must stay alive between calls. Drive it with "
            "tmux-send and read its output with tmux-recv. Idempotent: returns ok if the "
            "session already exists."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Session name (ASCII letters/digits/-/_, max 24)"},
                "command": {"type": "string", "description": "(Optional) command to launch inside the session instead of a shell.", "default": ""},
            },
            "required": ["name"],
        },
    },
    {
        "name": "tmux-send",
        "description": (
            "Send keystrokes to a persistent tmux session (created via tmux-new). By default "
            "a trailing newline is sent (Enter). Capture the response with tmux-recv. "
            "For raw-binary or sensitive input, pass through the session — output is never "
            "written to disk."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Session name created by tmux-new"},
                "keys": {"type": "string", "description": "Keystrokes to inject (e.g. a CLI command)."},
                "enter": {"type": "boolean", "description": "Append a newline (Enter) after the keystrokes. Default true.", "default": True},
            },
            "required": ["name", "keys"],
        },
    },
    {
        "name": "tmux-recv",
        "description": (
            "Return the recent scrollback of a persistent tmux session as a string. "
            "Bounded to the last `sinceLines` lines (default 50). Use the returned output "
            "as-is; do not rely on it persisting in a file."
        ),
        "inputSchema": {
            "type": "object",
            "properties": {
                "name": {"type": "string", "description": "Session name"},
                "sinceLines": {"type": "integer", "description": "How many recent lines to capture. Default 50.", "default": 50, "minimum": 1, "maximum": 500},
            },
            "required": ["name"],
        },
    },
    {
        "name": "tmux-kill",
        "description": "Terminate a persistent tmux session and free its pane. Safe to call on a non-existent session (returns ok).",
        "inputSchema": {"type": "object", "properties": {"name": {"type": "string", "description": "Session name"}}, "required": ["name"]},
    },
    # ── RECON / OSINT ──────────────────────────────────────────────────────────
    {"name": "masscan-scan", "description": "High-speed TCP/SYN port scan of an HTB target subnet (use a low --rate).",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Lab IP or CIDR"},
         "ports": {"type": "string", "description": "masscan -p range, e.g. '0-1000'", "default": "0-1000"},
         "rate": {"type": "string", "description": "packets/sec cap, e.g. '1000'", "default": "1000"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "rustscan-fast", "description": "Fast recon: quick port grab then nmap -sV. Best for a single host.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Lab IP"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "subfinder-enum", "description": "Subdomain discovery for a target domain (needs DNS egress).",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Domain, e.g. target.htb"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "httpx-probe", "description": "Probe a list of hosts/domains for live HTTP servers, titles and tech.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Host, domain, or newline/CI list of hosts"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "amass-enum", "description": "OSINT subdomain enumeration (enum4linux-ng/Passive/Active) for a domain.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Domain"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "feroxbuster", "description": "Directory/file/param fuzzing (force-directed) against a URL.",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL, e.g. http://10.10.x.x/"},
         "wordlist": {"type": "string", "description": "Wordlist path on the pwnbox, or a built-in ('common')"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "whatweb-fingerprint", "description": (
        "Web technology fingerprinting (CMS, plugins, headers) of a URL. Uses "
        "--no-errors and a per-URI timeout to avoid hanging on non-HTTP TLS "
        "listeners (e.g. mTLS C2 ports that return 0 bytes)."),
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "nuclei-scan", "description": "Template-based vulnerability scan (CVEs / misconfig / exposures). Pass a target URL.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Target URL or CIDR, e.g. http://10.10.x.x"},
         "templates": {"type": "string", "description": "Nuclei -it tag/category (default 'cves,misconfig,exposures')", "default": "cves,misconfig,exposures"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    # ── WEB VULN ───────────────────────────────────────────────────────────────
    {"name": "ffuf-fuzz", "description": "ffuf fuzzing for directories/files/vhosts/params against a web target.",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL with FUZZ placeholder, e.g. http://host/FUZZ"},
         "wordlist": {"type": "string", "description": "Wordlist path or built-in name ('common')"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "sqlmap-inject", "description": "SQLi detection & light exploitation (--batch, level capped). Abort on auth prompt.",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL (may include ?param= or POST data hint)"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "nikto-scan", "description": "Web server scanner for common misconfigs, CGI, plugins.",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "wpscan-analyze", "description": "WordPress security scanner (users, plugins, themes, config leaks).",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target WP URL"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "commix-inject", "description": "Automated command injection detection/exploitation (OWASP commix).",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    {"name": "xsstrike-scan", "description": "Reflected/stored XSS detection & payload analysis (XSStrike).",
     "inputSchema": {"type": "object", "properties": {
         "url": {"type": "string", "description": "Target URL with a parameter to test"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["url"]}},
    # ── EXPLOITATION ───────────────────────────────────────────────────────────
    {"name": "searchsploit-lookup", "description": "Search the local ExploitDB for a product/CVE term and list matches. (slim image has no ExploitDB; use the blackcart image tag.)",
     "inputSchema": {"type": "object", "properties": {
         "query": {"type": "string", "description": "Search term, e.g. 'vsftpd 2.3.4'"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["query"]}},
    {"name": "fetch-exploit", "description": (
        "Acquire an exploit file into the /tmp/exploits sandbox. Either query=term "
        "(searchsploit lookup → copies the matched ExploitDB file; requires the blackcart "
        "image, which ships ExploitDB) or url=<allowlisted URL> (curl into the sandbox; "
        "requires pwnboxEgress.internetRecon + EXPLOIT_FETCH_ALLOWLIST). Remote URLs are "
        "DENIED unless host is in EXPLOIT_FETCH_ALLOWLIST. Returns the on-disk path for run-exploit."),
     "inputSchema": {"type": "object", "properties": {
         "query": {"type": "string", "description": "Searchsploit search term (local ExploitDB)."},
         "url": {"type": "string", "description": "Allowlisted URL to download."},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": []}},
    {"name": "run-exploit", "description": (
        "Execute exploit code the agent authored, inside the /tmp/exploits sandbox. "
        "Writes `code` to a PID-suffixed file and runs it via the chosen interpreter "
        "against `target`. language ∈ python|python3|bash|sh|ruby|perl. "
        "Denies unknown interpreters; output truncated to ~16k chars."),
     "inputSchema": {"type": "object", "properties": {
         "code": {"type": "string", "description": "Full exploit/payload source code."},
         "language": {"type": "string", "description": "python|python3|bash|sh|ruby|perl", "default": "python"},
         "target": {"type": "string", "description": "Lab IP/host passed as argv to the script."},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["code"]}},
    {"name": "msfvenom-payload", "description": "Generate a payload with msfvenom (meterpreter/reverse/reverse shell).",
     "inputSchema": {"type": "object", "properties": {
            "payload": {"type": "string", "description": "msfvenom -p payload, e.g. linux/x64/meterpreter/reverse_tcp"},
            "lhost": {"type": "string", "description": "Listener (pwnbox) IP"},
            "lport": {"type": "string", "description": "Listener port", "default": "4444"},
            "format": {"type": "string", "description": "Output format (raw|elf|exe|...)", "default": "raw"},
            "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
            "required": ["payload", "lhost"]}},
    {"name": "metasploit-exploit", "description": "Run an msfconsole exploit module via a generated resource script.",
     "inputSchema": {"type": "object", "properties": {
         "module": {"type": "string", "description": "Exploit module, e.g. exploit/unix/ftp/vsftpd_234_backdoor"},
         "rhosts": {"type": "string", "description": "Target RHOSTS"},
         "payload": {"type": "string", "description": "Payload module (default: module's default)"},
         "options": {"type": "string", "description": "Extra set/show commands, one per line, e.g. 'set LPORT 4444'"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["module", "rhosts"]}},
    {"name": "hydra-brute", "description": "Hydra brute-force a service. Provide newline-separated users/passes; rate-limited by default but can raise threads.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Target IP/host"},
         "service": {"type": "string", "description": "Service, e.g. ssh|ftp|smb|http-post-form"},
         "users": {"type": "string", "description": "Newline-separated username list"},
         "passes": {"type": "string", "description": "Newline-separated password list"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT},
         "threads": {"type": "integer", "description": "Parallel connection tasks (default 16). Raise for noisy targets, lower for lockout-prone services (ssh).", "default": 16}},
         "required": ["target", "service", "users", "passes"]}},
    {"name": "john-crack", "description": "Crack hashes with john the ripper using a wordlist.",
     "inputSchema": {"type": "object", "properties": {
         "hashfile": {"type": "string", "description": "Path to hash file on the pwnbox"},
         "wordlist": {"type": "string", "description": "Wordlist path (e.g. nginx, rockyou)"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["hashfile", "wordlist"]}},
    # ── NETWORK / AD LAYER ─────────────────────────────────────────────────────
    {"name": "netexec-scan", "description": "NetExec (CrackMapExec) auth-assisted enumeration of a service/target.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Target IP/CIDR"},
         "protocol": {"type": "string", "description": "Protocol, e.g. smb|winrm|ssh", "default": "smb"},
         "module": {"type": "string", "description": "NetExec module, e.g. 'local-groups auth'"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target", "protocol"]}},
    {"name": "rpcclient-enum", "description": "rpcclient null-session enumeration (users/shares/enumdomusers).",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Target IP"},
         "command": {"type": "string", "description": "rpcclient -U% command, e.g. enumdomusers"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "smbmap-scan", "description": "smbmap enumeration of shares/permissions on a target.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Target IP"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "bloodhound-collect", "description": "Collect AD data (bloodhound-python) for graph analysis. Creds via env (BLOODHOUND_*).",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Domain controller IP/FQDN"},
         "domain": {"type": "string", "description": "AD domain"},
         "username": {"type": "string", "description": "Domain user (SharpHound collides as you)"},
         "password": {"type": "string", "description": "Domain password (prefer secretRef; 0-interaction if omitted)"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target", "domain"]}},
    # ── CLOUD / IaC POSTURE ────────────────────────────────────────────────────
    {"name": "trivy-scan", "description": "Trivy vuln/misconfig scan of a filesystem path or OCI image ref.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Filesystem path or image ref (e.g. ./srv or myapp:1.0)"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "checkov-iac", "description": "Checkov IaC scan for cloud misconfigurations in a Terraform/Tofu dir.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Directory of IaC to scan"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    # ── FORENSICS / SECRET HUNTING ─────────────────────────────────────────────
    {"name": "gitleaks-scan", "description": "gitleaks scan for secrets/credentials in a repo path or directory.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "Repo/directory path"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
    {"name": "volatility-analyze", "description": "Volatility3 plugin run against a memory image (e.g. pslist).",
     "inputSchema": {"type": "object", "properties": {
         "image": {"type": "string", "description": "Path to memory image"},
         "plugin": {"type": "string", "description": "Volatility plugin, e.g. windows.pslist", "default": "windows.pslist"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["image"]}},
    {"name": "yara-scan", "description": "Scan a file/dir for YARA rule matches.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "File or directory to scan"},
         "rules": {"type": "string", "description": "YARA rule/path to scan with"},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target", "rules"]}},
    {"name": "binwalk-analyze", "description": "binwalk signature scan + firmware extraction of a file.",
     "inputSchema": {"type": "object", "properties": {
         "target": {"type": "string", "description": "File path to analyze"},
         "extract": {"type": "boolean", "description": "Run extraction (-e) if true", "default": False},
         "timeout": {"type": "integer", "description": f"(1-{TOOL_TIMEOUT}) default {DEFAULT_TOOL_TIMEOUT}", "default": DEFAULT_TOOL_TIMEOUT}},
         "required": ["target"]}},
]


# ── Tool execution (real binaries, over tun0) ──────────────────────────────────

def clamp_timeout(requested):
    """Clamp a caller-requested timeout (seconds) into [1, TOOL_TIMEOUT].
    A non-positive or missing value falls back to the 1h ceiling so a misbehaving
    agent call can't request 0s; an oversized value is capped at TOOL_TIMEOUT."""
    try:
        requested = int(requested)
    except (TypeError, ValueError):
        return TOOL_TIMEOUT
    if requested <= 0 or requested > TOOL_TIMEOUT:
        return TOOL_TIMEOUT
    return requested

def _tun_up():
    return os.path.isdir("/sys/class/net/tun0")

def _require_tun():
    """Returns an error dict if the HTB tunnel isn't up, else None."""
    if not _tun_up():
        return {"ok": False, "error": "tun0 not up — the HTB OpenVPN tunnel is not established. "
                "Check /tmp/openvpn.log in the pwnbox pod: kubectl -n red-team logs deploy/red-pwnbox -c pwnbox"}

def _run(cmd, timeout=DEFAULT_TOOL_TIMEOUT):
    print(f"[pwnbox-mcp] $ {' '.join(cmd)} (timeout {timeout}s, tun0={_tun_up()})", file=sys.stderr, flush=True)
    try:
        p = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return (p.stdout or "") + (p.stderr or "")
    except subprocess.TimeoutExpired as e:
        partial = (e.stdout or e.stderr or "")[:4000]
        return f"TIMEOUT after {timeout}s (partial output below). Narrow the scope or lower `timeout` and re-run.\n{partial}"
    except FileNotFoundError:
        return f"TOOL MISSING: {cmd[0]}"

def _have(binary):
    """True if an executable is on PATH (used to give actionable messages when a
    tool is present on blackcart but absent on the slim image)."""
    return shutil.which(binary) is not None


def _effective_cpus():
    """Return (cpu_count, affinity_count, cgroup_quota) so the agent can see
    whether it's CPU-starved.  os.cpu_count() reports the host total, which in a
    container is often far more than what's actually schedulable — the last
    engagement assumed 18 cores but got ~1, making 14 M-word DCC2 cracking
    look feasible when it wasn't."""
    import multiprocessing as _mp
    try:
        affinity = len(_mp.cpu_count()) if hasattr(_mp, 'cpu_count') else _mp.cpu_count()
    except Exception:
        affinity = None
    try:
        affinity = len(os.sched_getaffinity(0))
    except (AttributeError, OSError):
        affinity = affinity or _mp.cpu_count()
    # cgroup v2 cpu.max quota (e.g. "20000 100000" → 0.2 CPUs)
    quota = None
    for p in ("/sys/fs/cgroup/cpu.max", "/sys/fs/cgroup/cpu/cpu.cfs_quota_us"):
        try:
            with open(p) as f:
                val = f.read().strip()
            if p.endswith("cpu.max"):
                parts = val.split()
                if len(parts) == 2 and parts[0] != "max":
                    quota = int(parts[0]) / int(parts[1])
            else:
                q = int(val)
                quota = max(0, q / 100000) if q > 0 else None
        except Exception:
            pass
    return {"cpu_count": _mp.cpu_count(), "affinity": affinity, "cgroup_quota": quota}

def probe_target(target, ports="1-1000", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun():
        return e
    # -sS SYN + -sV versions + -T4 speed. Capped at TOOL_TIMEOUT (< router's 1h).
    return {"ok": True, "tool": "probe-target",
            "output": _run(["nmap", "-Pn", "-sS", "-sV", "-T4", "--open", "-p",
                            str(ports), str(target)], timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def enum_shares(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun():
        return e
    return {"ok": True, "tool": "enum-shares",
            "output": _run(["enum4linux-ng", "-A", str(target)], timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def shell_exploit(technique, target, payload="", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun():
        return e
    technique = str(technique)
    target = str(target)
    args = payload.split() if payload else []
    # nmap NSE script exploit: -Pn --script <technique>
    cmd = ["nmap", "-Pn", "-T4", "--script", technique, str(target)] + args
    return {"ok": True, "tool": "shell-exploit",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Recon / OSINT handlers ─────────────────────────────────────────────────────

def masscan_scan(target, ports="1-1000", rate="1000", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "masscan-scan",
            "output": _run(["masscan", "-p", str(ports), "--rate", str(rate), str(target)],
                           timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def rustscan_fast(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "rustscan-fast",
            "output": _run(["rustscan", "-a", str(target), "--", "-sV", "-T4"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def subfinder_enum(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "subfinder-enum",
            "output": _run(["subfinder", "-d", str(target), "-silent"],
                           timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def httpx_probe(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "httpx-probe",
            "output": _run(["httpx", "-l", str(target), "-status-code", "-title", "-tech-detect"],
                           timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def amass_enum(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "amass-enum",
            "output": _run(["amass", "enum", "-passive", "-d", str(target)],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def feroxbuster(url, wordlist="common", timeout=DEFAULT_TOOL_TIMEOUT):
    wl = _wordlist(wordlist)
    if e := _require_tun(): return e
    if wl is None and wordlist != "common":
        return {"ok": False, "error": f"wordlist not found: {wordlist}"}
    cmd = ["feroxbuster", "-u", str(url), "-t", "10"]
    if wl: cmd += ["-w", wl]
    return {"ok": True, "tool": "feroxbuster",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def whatweb_fingerprint(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    # --no-errors: don't hang on non-HTTP TLS ports (e.g. mTLS C2 listeners).
    # --timeout: per-URI connection timeout (default 300s is far too long for
    # one-shot fingerprinting; cap at the caller's timeout floor).
    to = max(10, min(int(timeout or 30), 30)) if isinstance(timeout, int) else 15
    return {"ok": True, "tool": "whatweb-fingerprint",
            "output": _run(["whatweb", "-v", "--no-errors", f"--timeout={to}",
                            str(url)], timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def nuclei_scan(target, templates="cves,misconfig,exposures", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    # -it = include templates (tag/category); nuclei downloads latest templates on first run.
    cmd = ["nuclei", "-u", str(target), "-it", str(templates), "-silent"]
    return {"ok": True, "tool": "nuclei-scan",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Web vuln handlers ─────────────────────────────────────────────────────────

def ffuf_fuzz(url, wordlist="common", timeout=DEFAULT_TOOL_TIMEOUT):
    wl = _wordlist(wordlist)
    if e := _require_tun(): return e
    if wl is None and wordlist != "common":
        return {"ok": False, "error": f"wordlist not found: {wordlist}"}
    cmd = ["ffuf", "-u", str(url), "-t", "10", "-mc", "200,403,401,302"]
    if wl: cmd += ["-w", wl]
    return {"ok": True, "tool": "ffuf-fuzz",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def sqlmap_inject(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    # --batch + level 3 cap keeps it bounded; abort on auth prompts.
    return {"ok": True, "tool": "sqlmap-inject",
            "output": _run(["sqlmap", "-u", str(url), "--batch", "--level=3",
                            "--risk=1", "--threads=4", "--timeout=10"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def nikto_scan(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "nikto-scan",
            "output": _run(["nikto", "-h", str(url), "-Tuning", "123456"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def wpscan_analyze(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "wpscan-analyze",
            "output": _run(["wpscan", "--url", str(url), "--no-banner"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def commix_inject(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "commix-inject",
            "output": _run(["commix", "--url", str(url), "--batch", "--level=3"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def xsstrike_scan(url, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "xsstrike-scan",
            "output": _run(["python3", "-m", "XSStrike", "--single", str(url)],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Exploitation handlers ──────────────────────────────────────────────────────

def searchsploit_lookup(query, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    if not _have("searchsploit"):
        return {"ok": False, "tool": "searchsploit-lookup",
                "error": ("searchsploit / ExploitDB is not present in the slim image. "
                          "Build with the blackcart image tag (pwnbox.image.slim: false), "
                          "which ships the ExploitDB DB; or fetch a specific exploit via "
                          "fetch-exploit(url=<allowlisted URL>) with pwnboxEgress.internetRecon.")}
    return {"ok": True, "tool": "searchsploit-lookup",
            "output": _run(["searchsploit", "-t", str(query)], timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def fetch_exploit(query="", url="", target="", timeout=DEFAULT_TOOL_TIMEOUT):
    """Acquire an exploit file into EXPLOIT_DIR.
    - query=<term>: searchsploit lookup; copy first matched ExploitDB file into the
      sandbox and return its path.
    - url=<allowlisted URL>: curl -fsSL into the sandbox; DENIED if host not allowed.
    """
    if (e := _require_tun()): return e
    os.makedirs(EXPLOIT_DIR, exist_ok=True)
    if url:
        u = str(url)
        if not _host_allowed(u):
            return {"ok": False, "error": f"fetch URL host not in EXPLOIT_FETCH_ALLOWLIST: {u}"}
        path = os.path.join(EXPLOIT_DIR, os.path.basename(u.split("?")[0]) or "exploit")
        out = _run(["curl", "-fsSL", "-o", path, u], timeout=clamp_timeout(timeout))
        return {"ok": os.path.isfile(path), "path": path, "output": out[-4000:]}
    if not query:
        return {"ok": False, "error": "fetch-exploit requires either a `query` or an allowlisted `url`"}
    if not _have("searchsploit"):
        return {"ok": False, "path": None,
                "error": ("searchsploit/ExploitDB is not present in the slim image. "
                          "On the blackcart image tag the query= path copies the matched "
                          "ExploitDB file into the sandbox. Here, fetch a specific exploit "
                          "via fetch-exploit(url=<allowlisted URL>) instead.")}
    out = _run(["searchsploit", "-t", "-w", str(query)], timeout=clamp_timeout(timeout))
    path = None
    for line in out.splitlines():
        # searchsploit -t -w prints: | Exploits/... | <path>
        if "Exploits/" in line or "/usr/share/exploitdb/" in line:
            candidate = line.strip().split("|")[-1].strip()
            cand_full = candidate if candidate.startswith("/") else os.path.join("/usr/share/exploitdb", candidate)
            if os.path.isfile(cand_full):
                path = cand_full
                break
    if not path:
        return {"ok": False, "path": None, "output": out[-TOUT_FETCH:]}
    dst = os.path.join(EXPLOIT_DIR, os.path.basename(path))
    _run(["cp", path, dst])
    return {"ok": True, "path": dst, "output": f"copied {path} -> {dst}\n{out[-TOUT_FETCH:]}"}

def run_exploit(code="", language="python", target="", timeout=DEFAULT_TOOL_TIMEOUT):
    """Execute agent-authored exploit code in the EXPLOIT_DIR sandbox.
    Writes `code` to a PID-suffixed file (chmod 700) then runs it via the chosen
    interpreter. language ∈ python|python3|bash|sh|ruby|perl. Denies unknown/missing
    interpreters. Output truncated to TOUT_PUSH."""
    if (e := _require_tun()): return e
    if not code:
        return {"ok": False, "error": "no code supplied"}
    interp = {"python": "python3", "python3": "python3", "bash": "bash",
              "sh": "bash", "ruby": "ruby", "perl": "perl"}.get(language.lower())
    if not interp:
        return {"ok": False, "error": f"unsupported language: {language}"}
    if not shutil.which(interp):
        return {"ok": False, "error": f"interpreter not installed in pwnbox image: {interp}"}
    os.makedirs(EXPLOIT_DIR, exist_ok=True)
    ext = ".py" if interp == "python3" else (".sh" if interp == "bash" else f".{language}")
    path = os.path.join(EXPLOIT_DIR, f"agent_exploit_{os.getpid()}{ext}")
    with open(path, "w") as f:
        f.write(code)
    os.chmod(path, 0o700)
    # Pass `target` as argv[1]/$1 to every interpreter so the agent's script can read it.
    cmd = [interp, path, str(target)]
    return {"ok": True, "tool": "run-exploit",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def msfvenom_payload(payload, lhost, lport="4444", format="raw", timeout=DEFAULT_TOOL_TIMEOUT):
    if (e := _require_tun()): return e
    cmd = ["msfvenom", "-p", str(payload), f"LHOST={str(lhost)}", f"LPORT={str(lport)}", "-f", str(format)]
    return {"ok": True, "tool": "msfvenom-payload",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def metasploit_exploit(module, rhosts, payload="", options="", timeout=DEFAULT_TOOL_TIMEOUT):
    if (e := _require_tun()): return e
    # Build a resource script so msfconsole runs non-interactively.
    rc = f"use {module}\nset RHOSTS {rhosts}\n"
    if payload:
        rc += f"set PAYLOAD {payload}\n"
    for line in str(options).splitlines():
        line = line.strip()
        if line:
            rc += f"{line}\n"
    rc += "exploit\nexit\n"
    rc_path = os.path.join(EXPLOIT_DIR, f"msf_{os.getpid()}.rc")
    os.makedirs(EXPLOIT_DIR, exist_ok=True)
    with open(rc_path, "w") as f:
        f.write(rc)
    return {"ok": True, "tool": "metasploit-exploit",
            "output": _run(["msfconsole", "-q", "-r", rc_path],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def hydra_brute(target, service, users, passes, timeout=DEFAULT_TOOL_TIMEOUT, threads=16):
    if e := _require_tun(): return e
    # Write the (agent-supplied) wordlists to temp files inside the sandbox.
    os.makedirs(EXPLOIT_DIR, exist_ok=True)
    up = os.path.join(EXPLOIT_DIR, f"users_{os.getpid()}.lst")
    pp = os.path.join(EXPLOIT_DIR, f"passes_{os.getpid()}.lst")
    with open(up, "w") as f: f.write(users)
    with open(pp, "w") as f: f.write(passes)
    os.chmod(up, 0o600); os.chmod(pp, 0o600)
    try:
        t = max(1, int(threads))
    except (TypeError, ValueError):
        t = 16
    return {"ok": True, "tool": "hydra-brute",
            "output": _run(["hydra", "-L", up, "-P", pp, "-t", str(t), "-f",
                            f"{service}://{target}"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def john_crack(hashfile, wordlist, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "john-crack",
            "output": _run(["john", f"--wordlist={wordlist}", str(hashfile)],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Network / AD layer handlers ────────────────────────────────────────────────

def netexec_scan(target, protocol="smb", module="", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    # Credentials come from operator-mounted secrets (env), NOT from the agent call,
    # so plaintext domain creds never cross the model-router boundary.
    user = os.environ.get("NETEXEC_USER", "")
    pwd = os.environ.get("NETEXEC_PASS", "")
    domain = os.environ.get("NETEXEC_DOMAIN", "")
    cmd = ["netexec", str(target), str(protocol)]
    if user:
        cmd += ["-u", user]
    if pwd:
        cmd += ["-p", pwd]
    if domain:
        cmd += ["-d", domain]
    if module:
        for m in module.split():
            cmd += ["-M", m]
    return {"ok": True, "tool": "netexec-scan",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def rpcclient_enum(target, command="enumdomusers", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "rpcclient-enum",
            "output": _run(["rpcclient", "-U", "", "//"+str(target), "-c", str(command)],
                           timeout=clamp_timeout(timeout))[-TOUT_PULL:]}

def smbmap_scan(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "smbmap-scan",
            "output": _run(["smbmap", "-H", str(target), "-r"], timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def bloodhound_collect(target, domain, username="", password="", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    # Prefer operator-mounted secrets (env) over args so plaintext AD creds never
    # cross the model-router boundary; fall back to the (rarely used) call args only.
    user = os.environ.get("BLOODHOUND_USER", username or "")
    pwd = os.environ.get("BLOODHOUND_PASS", password or "")
    dom = str(domain) or os.environ.get("BLOODHOUND_DOMAIN", "")
    cmd = ["bloodhound-python", "-u", user or "anonymous",
           "-p", pwd, "-d", dom, "-c", "all",
           "-ns", str(target), "-o", EXPLOIT_DIR]
    return {"ok": True, "tool": "bloodhound-collect",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Cloud / IaC posture handlers ──────────────────────────────────────────────

def trivy_scan(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "trivy-scan",
            "output": _run(["trivy", "fs", "--scanners", "vuln,config", str(target)],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def checkov_iac(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "checkov-iac",
            "output": _run(["checkov", "-d", str(target), "-s"], timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

# ── Forensics / secret hunting handlers ───────────────────────────────────────

def gitleaks_scan(target, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "gitleaks-scan",
            "output": _run(["gitleaks", "detect", "--source", str(target), "--report-format", "text"],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def volatility_analyze(image, plugin="windows.pslist", timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "volatility-analyze",
            "output": _run(["volatility3", "-f", str(image), str(plugin)],
                           timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def yara_scan(target, rules, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    return {"ok": True, "tool": "yara-scan",
            "output": _run(["yara", "-r", str(rules), str(target)], timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}

def binwalk_analyze(target, extract=False, timeout=DEFAULT_TOOL_TIMEOUT):
    if e := _require_tun(): return e
    cmd = ["binwalk", "--signature"]
    if extract:
        cmd.append("-e")
    cmd.append(str(target))
    return {"ok": True, "tool": "binwalk-analyze",
            "output": _run(cmd, timeout=clamp_timeout(timeout))[-TOUT_PUSH:]}


# ── Shared helpers ─────────────────────────────────────────────────────────────

def _wordlist(name):
    """Resolve a built-in wordlist name to a path on the pwnbox, or None."""
    if not name:
        return None
    builtins = {
        "common": "/usr/share/wordlists/common.txt",
    }
    candidate = builtins.get(str(name))
    if candidate and os.path.isfile(candidate):
        return candidate
    # allow explicit absolute paths
    if os.path.isfile(str(name)):
        return str(name)
    return candidate  # may be None


def _host_allowed(url):
    """True if `url`'s host is in the (case-insensitive) EXPLOIT_FETCH_ALLOWLIST."""
    if not ALLOWED_FETCH_HOSTS:
        return False
    try:
        host = urlparse(url).hostname or ""
    except Exception:
        return False
    return host.lower() in ALLOWED_FETCH_HOSTS


REDIS_ADDR = os.environ.get("REDIS_ADDRESS", "")
REDIS_PASSWORD = os.environ.get("REDIS_PASSWORD", "")

# In-process dedup set for submit-findings when Redis is unavailable.  This is
# session-scoped (ephemeral), so it only catches re-submissions within a single
# engagement run; Redis is the authoritative cross-session store.
_DEDUP_SEEN: set = set()

def submit_findings(flag, details):
    """Record a captured flag.  Deduplicates against flags already recorded in
    the arena-events Redis stream so the same flag is never scored twice even if
    the agent re-submits (seen in the last engagement: both flags were submitted
    twice).  When Redis is unavailable, dedup falls back to an in-process set
    (session-scoped) so at least within a single run duplicates are dropped."""
    global _DEDUP_SEEN
    rec = {"ts": time.time(), "flag": str(flag), "details": details, "source": "red-pwnbox"}
    line = json.dumps(rec)

    # 1) Check in-process dedup first (fast, no I/O).
    already = str(flag) in _DEDUP_SEEN
    _DEDUP_SEEN.add(str(flag))

    # 2) Check Redis-backed dedup (cross-session, authoritative).
    if not already and REDIS_ADDR:
        try:
            _cnt, recorded = _recorded_flags()
            if str(flag) in [str(f) for f in recorded]:
                already = True
        except Exception:  # noqa
            pass

    # 3) Only XADD if this is a genuinely new flag.
    if REDIS_ADDR and not already:
        try:
            _redis_xadd(REDIS_ADDR, REDIS_PASSWORD, "arena-events", line)
        except Exception as e:  # noqa
            print(f"[pwnbox-mcp] redis publish failed: {e}", file=sys.stderr, flush=True)

    print(f"[finding] {line}", flush=True)
    if already:
        return {"status": "duplicate", "flag": flag,
                "message": "flag already recorded — not re-submitted to avoid double-scoring"}
    return {"status": "recorded", "flag": flag}


def _redis_cmd(addr, password, cmd, *args, timeout=10):
    """Send an arbitrary RESP command to Redis and return the decoded reply.

    Companion to _redis_xadd's sender; lets engagement-state/flag-harvest READ
    recorded findings (XLEN) instead of only writing them."""
    import socket
    host, port, pw = _parse_redis_addr(addr, password)
    def bulk(s):
        b = s.encode() if isinstance(s, str) else s
        return f"${len(b)}\r\n".encode() + b + b"\r\n"
    def arr(n, *members):
        out = f"*{n}\r\n".encode()
        for m in members:
            out += m
        return out
    req = arr(len(args) + 1, bulk(cmd), *[bulk(a) for a in args])
    with socket.create_connection((host, port), timeout=timeout) as s:
        s.settimeout(timeout)
        if pw:
            s.sendall(arr(2, bulk("AUTH"), bulk(pw)))
        s.sendall(req)
        return _readReply(s, timeout=timeout)

def _recorded_flags(pattern=None):
    """Return (count, [flag strings]) already recorded in arena-events.

    Prefers `redis-cli` (reliable parsing) and falls back to a raw XLEN over the
    socket for the count only. Always fails closed to (0, []) — dedup is a
    convenience, never a correctness gate. `pattern` is the flag regex used to
    filter recorded entries (defaults to a broad CTF flag pattern, NOT a
    specific box's format)."""
    if not REDIS_ADDR:
        return None, []
    flags = []
    if pattern is None:
        pattern = DEFAULT_FLAG_PATTERN
    # Prefer redis-cli: parse XREVRANGE output for flags.
    if _have("redis-cli"):
        host, port, pw = _parse_redis_addr(REDIS_ADDR, REDIS_PASSWORD)
        base = ["redis-cli"]
        if pw:
            base += ["-a", pw, "--no-auth-warning"]
        base += ["-h", host, "-p", str(port)]
        c = _run(base + ["XLEN", "arena-events"], timeout=15)
        try:
            cnt = int(re.search(r"\d+", c).group())
        except Exception:  # noqa
            cnt = 0
        r = _run(base + ["XREVRANGE", "arena-events", "+", "-", "COUNT", "20"], timeout=15)
        flags = re.findall(pattern, r)
        return cnt, flags
    # Fallback: raw socket XLEN only (scalar; _readReply handles it).
    try:
        cnt = _redis_cmd(REDIS_ADDR, REDIS_PASSWORD, "XLEN", "arena-events")
        cnt = int(cnt) if (cnt or "").strip().lstrip("-").isdigit() else 0
    except Exception:  # noqa
        cnt = 0
    return cnt, flags


# Broad, box-agnostic flag pattern: matches CAPITALIZED_TAG{32+ hex} (HTB{},
# PUPPET{}, etc.) without hard-coding any one machine's format. The agent may
# be handed a narrower pattern (e.g. the engagement's known flag format) via the
# `flag-harvest`/`submit-findings` `pattern` argument.
DEFAULT_FLAG_PATTERN = r"[A-Z0-9_-]+\{[a-f0-9]{32}\}"

def flag_harvest(target="/tmp", pattern=None, timeout=DEFAULT_TOOL_TIMEOUT):
    """Search the pwnbox filesystem + redis stream for exfiltrated flags.

    `pattern` is a regex for the flag format. Defaults to a BROAD CTEF pattern
    (`[A-Z0-9_-]+\\{[a-f0-9]{32}}`) — NOT hard-coded to any one box's format
    (the old code only matched `PUPPET{...}`, which made it blind to `HTB{}`,
    `monk{...}`, etc.). Pass the engagement's known format (e.g. `PUPPET\\{...}`)
    only when you have confirmed it."""
    if (e := _require_tun()):
        return e
    target = str(target)
    pattern = pattern or DEFAULT_FLAG_PATTERN
    text_hits, bin_hits = [], []
    # 1) recursive grep for the flag pattern in text files (binary-safe via -a).
    out = _run(["grep", "-rIo", "-m", "500", "--", pattern, target],
               timeout=clamp_timeout(timeout))
    for line in out.splitlines():
        if re.search(pattern, line):
            text_hits.append(line.strip()[:200])
    # 2) string-scan any executables/binaries in the target dir for the pattern.
    if os.path.isdir(target):
        for root, _dirs, files in os.walk(target):
            for fn in files:
                p = os.path.join(root, fn)
                if not os.path.isfile(p) or os.path.islink(p):
                    continue
                if _have("strings") and _looks_binary(p):
                    if os.path.getsize(p) > 20_000_000:
                        continue  # don't strings a 15MB PE inline
                    sout = _run(["strings", "--", p], timeout=min(clamp_timeout(timeout), 60))
                    for line in sout.splitlines():
                        for m in re.findall(pattern, line):
                            bin_hits.append(f"{p}: {m[:120]}")
                            if len(bin_hits) >= 50:
                                break
                    if len(bin_hits) >= 50:
                        break
            if len(bin_hits) >= 50:
                break
    # 3) flags already recorded via submit-findings in the redis stream.
    cnt, recorded = _recorded_flags(pattern)
    return {
        "ok": True,
        "tool": "flag-harvest",
        "target": target,
        "pattern": pattern,
        "text_matches": text_hits[:50],
        "binary_matches": bin_hits[:50],
        "redis_recorded_count": cnt,
        "redis_recorded_flags": recorded,
        "summary": (f"text={len(text_hits)} binary={len(bin_hits)} "
                    f"redis_recorded={cnt} known_flags={len(recorded)}"),
    }

def _looks_binary(path, sample=2048):
    """Heuristic: does this file contain NUL bytes / look like an ELF/PE/ELF?"""
    try:
        with open(path, "rb") as f:
            chunk = f.read(sample)
    except OSError:
        return False
    if b"\x00" in chunk:
        return True
    return chunk[:4] in (b"\x7fELF", b"MZ")  # ELF / PE

def engagement_state(timeout=DEFAULT_TOOL_TIMEOUT):
    """Compact engagement snapshot — avoid loading the multi-MB trajectory."""
    snap = {"tun0_up": _tun_up(), "tools": {}}
    # openvpn status
    try:
        if os.path.isfile("/tmp/openvpn.log"):
            with open("/tmp/openvpn.log") as f:
                snap["openvpn_log_tail"] = "".join(f.readlines()[-5:]).strip()[-800:]
    except Exception:  # noqa
        snap["openvpn_log_tail"] = "unreadable"
    # exploit workspace
    wspace = os.environ.get("EXPLOIT_DIR", "/tmp/exploits")
    files = []
    if os.path.isdir(wspace):
        for root, _dirs, fs in os.walk(wspace):
            for fn in fs:
                p = os.path.join(root, fn)
                try:
                    files.append({"path": p, "bytes": os.path.getsize(p)})
                except OSError:
                    pass
    snap["exploit_workspace"] = {"dir": wspace, "file_count": len(files),
                                 "files": sorted(files, key=lambda x: -x["bytes"])[:15]}
    # C2 framework presence (generic: detect whether *a* C2 client binary is
    # installed on the pwnbox, without branding one framework in the state key).
    snap["c2"] = {"client_available": _have("sliver")}
    # CPU count: report BOTH os.cpu_count() (often inflated in containers) and
    # the cgroup/ affinity-limited count so the agent can right-size cracking
    # time estimates (last engagement: os.cpu_count()==18 but ~1 effective core).
    snap["cpus"] = _effective_cpus()
    # redis findings
    cnt, flags = _recorded_flags()
    snap["recorded_findings"] = {"stream": "arena-events", "count": cnt, "flags": flags}
    # toolbelt self-check
    snap["tools"] = {b: _have(b) for b in ("nmap", "masscan", "ffuf", "nuclei",
                                           "sliver", "hydra", "sqlmap", "ncat")}
    return {"ok": True, "tool": "engagement-state", "state": snap}

def _parse_redis_addr(addr, password):
    """Resolve (host, port, password) from a redis URL or a bare host[:port].

    Accepts both `redis://:password@host:port` (the operator's REDIS_URL form,
    which may carry the password) and a bare `host:port` / `host`. An explicit
    `password` argument wins when the URL has none, so callers can pass
    REDIS_PASSWORD separately.
    """
    pw = password
    host = addr
    port = 6379
    if addr.startswith("redis://") or addr.startswith("rediss://"):
        from urllib.parse import urlparse
        u = urlparse(addr)
        if u.hostname:
            host = u.hostname
        if u.port:
            port = u.port
        if u.password and not pw:
            pw = u.password
    else:
        host, _, rest = addr.partition(":")
        if rest:
            try:
                port = int(rest)
            except ValueError:
                port = 6379
    return host, port, pw

def _readReply(sock, timeout=5.0):
    """Read one RESP reply from `sock` and return it as decoded text.

    Only the reply *shape* matters to us (we never act on its contents, only
    surface failures as errors): integer/bulk/status/arrays all terminate at the
    next `\r\n` boundary for status/integer, or after the declared bulk length
    for bulk/array replies. We drain up to the boundary and return.
    """
    sock.settimeout(timeout)
    buf = b""
    # Read until we hit a line boundary, then handle bulk/array payloads.
    while b"\r\n" not in buf:
        chunk = sock.recv(4096)
        if not chunk:
            break
        buf += chunk
        if len(buf) > 1 << 20:
            break  # safety: never read more than ~1MiB of reply
    line, _, rest = buf.partition(b"\r\n")
    if line.startswith(b"$-1") or line == b"+OK" or line.startswith(b"+") or line.startswith(b":"):
        return line.decode(errors="replace")
    if line.startswith(b"$"):  # bulk string
        try:
            n = int(line[1:])
        except ValueError:
            return line.decode(errors="replace")
        while len(rest) < n + 2:
            chunk = sock.recv(4096)
            if not chunk:
                break
            rest += chunk
        return rest[:n].decode(errors="replace")
    if line.startswith(b"*"):  # array — count nested bulk/int lines
        try:
            n = int(line[1:])
        except ValueError:
            return line.decode(errors="replace")
        out = [line.decode(errors="replace")]
        remaining = n
        while remaining > 0:
            sub, _, rest = rest.partition(b"\r\n") if b"\r\n" in rest else (rest, b"", b"")
            if sub == b"":
                sub = rest
                rest = b""
            if sub.startswith(b"$"):
                try:
                    sn = int(sub[1:])
                except ValueError:
                    out.append(sub.decode(errors="replace")); remaining -= 1; continue
                while len(rest) < sn + 2:
                    chunk = sock.recv(4096)
                    if not chunk:
                        break
                    rest += chunk
                out.append(rest[:sn].decode(errors="replace"))
                rest = rest[sn + 2:]
            else:
                out.append(sub.decode(errors="replace"))
            remaining -= 1
        return " ".join(out)
    return line.decode(errors="replace")

def _redis_xadd(addr, password, stream, payload):
    """XADD payload onto `stream` in Redis, authenticating first if a password is set.

    Connects to the host:port in `addr` (URL or bare), sends AUTH <password> when
    one is known, then XADD. A missing/wrong password surfaces as an AUTH error
    rather than a silent DNS-time failure, and never blocks the agent run (callers
    catch and log).
    """
    import socket
    host, port, pw = _parse_redis_addr(addr, password)
    def bulk(s):
        b = s.encode()
        return f"${len(b)}\r\n".encode() + b + b"\r\n"
    def send(cmd, *args):
        req = f"*{len(args) + 1}\r\n".encode() + bulk(cmd)
        for a in args:
            req += bulk(a)
        s.sendall(req)
        return _readReply(s)
    with socket.create_connection((host, port), timeout=10) as s:
        if pw:
            send("AUTH", pw)  # reply discarded; failure surfaces on the XADD as NOAUTH
        send("XADD", stream, "*", payload)

# ── Session / TCP handlers ───────────────────────────────────────────────────────
# Persistent interactive sessions (tmux) + raw TCP (ncat). These bridge the gap
# between agent-orc's one-shot tool model and long-lived/interactive red-teaming
# tooling (Sliver TUI, C2 sockets). Output is returned as the tool-result string —
# never written to disk (filesystem is unreliable in Kubernetes).

_SESS_NAME_RE = re.compile(r"^[A-Za-z0-9_-]{1,24}$")

def tmux_new(name, command=""):
    if not _have("tmux"):
        return {"ok": False, "error": "TOOL MISSING: tmux (image needs the tmux package)"}
    if not name or not _SESS_NAME_RE.match(str(name)):
        return {"ok": False, "error": "invalid session name (ASCII letters/digits/-/_, max 24)"}
    name = str(name)
    # Idempotent: if the session already exists, report ok (don't recreate/kill).
    check = subprocess.run(["tmux", "has-session", "-t", name],
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    if check.returncode == 0:
        return {"ok": True, "tool": "tmux-new", "session": name, "message": "session already exists"}
    cmd = ["tmux", "new-session", "-d", "-s", name, "-x", "240", "-y", "60"]
    if command:
        # Launch a command inside the session instead of a shell.
        cmd += ["--"] + shlex.split(str(command))
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=clamp_timeout(10))
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "tmux new-session timed out"}
    if out.returncode != 0:
        return {"ok": False, "error": f"tmux new-session failed: {(out.stderr or out.stdout or '').strip()}"}
    detail = "launched" if command else "shell"
    return {"ok": True, "tool": "tmux-new", "session": name, "message": f"session created ({detail})"}

def _tmux_missing(msg):
    """True if a tmux error indicates the session/server is absent (clean, idempotent)."""
    m = msg.lower()
    return "no server running" in m or "no session" in m or "not found" in m or "no such" in m

def tmux_send(name, keys, enter=True):
    if not _have("tmux"):
        return {"ok": False, "error": "TOOL MISSING: tmux"}
    name = str(name); keys = str(keys)
    if not name or not _SESS_NAME_RE.match(name):
        return {"ok": False, "error": "invalid session name"}
    cmd = ["tmux", "send-keys", "-t", name, keys]
    if enter:
        cmd.append("C-m")
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=clamp_timeout(10))
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "tmux send-keys timed out"}
    if out.returncode != 0:
        msg = (out.stderr or "").strip()
        if _tmux_missing(msg):
            return {"ok": True, "tool": "tmux-send", "session": name, "message": "session does not exist (create it with tmux-new first)"}
        return {"ok": False, "error": f"tmux send-keys failed: {msg}"}
    return {"ok": True, "tool": "tmux-send", "session": name, "sent": keys, "enter": bool(enter)}

def tmux_recv(name, sinceLines=50):
    if not _have("tmux"):
        return {"ok": False, "error": "TOOL MISSING: tmux"}
    name = str(name)
    if not name or not _SESS_NAME_RE.match(name):
        return {"ok": False, "error": "invalid session name"}
    try:
        sinceLines = max(1, min(int(sinceLines), 500))
    except (TypeError, ValueError):
        sinceLines = 50
    # capture-pane -p -S -N  -> from N lines before the bottom to the present.
    cmd = ["tmux", "capture-pane", "-p", "-S", f"-{sinceLines}", "-t", name]
    try:
        out = subprocess.run(cmd, capture_output=True, text=True, timeout=clamp_timeout(10))
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "tmux capture-pane timed out"}
    if out.returncode != 0:
        msg = (out.stderr or "").strip()
        if _tmux_missing(msg):
            return {"ok": True, "tool": "tmux-recv", "session": name, "message": "session does not exist (create it with tmux-new first)"}
        return {"ok": False, "error": f"tmux capture-pane failed: {msg}"}
    return {"ok": True, "tool": "tmux-recv", "session": name, "lines": sinceLines, "output": out.stdout}

def tmux_kill(name):
    if not _have("tmux"):
        return {"ok": False, "error": "TOOL MISSING: tmux"}
    name = str(name)
    if not name or not _SESS_NAME_RE.match(name):
        return {"ok": False, "error": "invalid session name"}
    try:
        out = subprocess.run(["tmux", "kill-session", "-t", name],
                             capture_output=True, text=True, timeout=clamp_timeout(10))
    except subprocess.TimeoutExpired:
        return {"ok": False, "error": "tmux kill-session timed out"}
    if out.returncode != 0:
        # kill-session errors if the session is absent — treat as ok (idempotent).
        msg = (out.stderr or "").strip()
        if "no sessions" in msg.lower() or "not found" in msg.lower():
            return {"ok": True, "tool": "tmux-kill", "session": name, "message": "session already gone"}
        return {"ok": False, "error": f"tmux kill-session failed: {msg}"}
    return {"ok": True, "tool": "tmux-kill", "session": name, "message": "killed"}

def _tcp_connect_pure_py(host, port, data="", timeout=30, ssl=False):
    """Pure-Python fallback for tcp_connect when ncat is absent (the slim
    Docker image does not ship ncat — Debian's `nmap` package does NOT include
    it; ncat is a separate package).  Uses the stdlib ssl+socket modules so the
    MCP tool stays functional without an image rebuild."""
    import socket as _socket
    def _truncate(s, n=8000):
        return s if len(s) <= n else s[:n] + f"\n[...truncated to {n} chars...]"
    try:
        sock = _socket.create_connection((str(host), int(port)), timeout=timeout)
    except _socket.gaierror as e:
        return {"ok": False, "error": f"DNS resolution failed for {host}: {e}"}
    except (_socket.timeout, TimeoutError) as e:
        return {"ok": False, "error": f"connection timed out to {host}:{port}: {e}"}
    except OSError as e:
        return {"ok": False, "error": f"connect failed to {host}:{port}: {e}",
                "ncat_fallback": True}
    try:
        if ssl:
            ctx = ssl.create_default_context()
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            sock = ctx.wrap_socket(sock, server_hostname=str(host))
        if data:
            sock.sendall(data.encode() if isinstance(data, str) else data)
        chunks = []
        sock.settimeout(timeout)
        while True:
            try:
                chunk = sock.recv(4096)
            except _socket.timeout:
                break
            if not chunk:
                break
            chunks.append(chunk)
            if sum(len(c) for c in chunks) > 65536:
                break
        out = b"".join(chunks).decode("utf-8", "replace")
    except Exception as e:
        return {"ok": False, "error": f"transfer error: {e}",
                "ncat_fallback": True}
    finally:
        try:
            sock.close()
        except Exception:
            pass
    return {"ok": True, "tool": "tcp-connect", "host": host, "port": port,
            "ssl": ssl, "timeout": timeout, "ncat_fallback": True,
            "output": _truncate(out)}


def tcp_connect(host, port, data="", timeout=30, ssl=False):
    if not host:
        return {"ok": False, "error": "host is required"}
    try:
        port = int(port)
    except (TypeError, ValueError):
        return {"ok": False, "error": "port must be an integer"}
    if not (1 <= port <= 65535):
        return {"ok": False, "error": "port out of range 1-65535"}
    try:
        timeout = max(1, min(int(timeout), 600))
    except (TypeError, ValueError):
        timeout = 30
    # ncat (ships with nmap) supports -w, --ssl, stdin->stdout relay.
    # If ncat is missing (slim image), fall back to a pure-Python socket — the
    # tool must still function.  See Dockerfile comment correcting that nmap does
    # NOT bundle ncat on Debian.
    if not _have("ncat"):
        return _tcp_connect_pure_py(host, port, data, timeout, ssl)
    cmd = ["ncat", "-w", str(timeout)]
    if ssl:
        cmd.append("--ssl")
    cmd += [str(host), str(port)]
    import socket as _socket
    def _truncate(s, n=8000):
        return s if len(s) <= n else s[:n] + f"\n[...truncated to {n} chars...]"
    try:
        p = subprocess.run(cmd, input=data or None, capture_output=True, text=True,
                           timeout=timeout + 2)
    except subprocess.TimeoutExpired as e:
        partial = (e.stdout or "" + (e.stderr or ""))[:4000]
        return {"ok": True, "tool": "tcp-connect", "host": host, "port": port,
                "ssl": ssl, "timedOut": True,
                "output": f"TIMEOUT after {timeout}s (partial output below).\n{partial}"}
    except _socket.gaierror as e:
        return {"ok": False, "error": f"DNS resolution failed for {host}: {e}"}
    out = (p.stdout or "")
    err = (p.stderr or "")
    # ncat writes nothing to stdout on a refused/silent connection; surface the
    # stderr diagnostics (connection refused, reset, etc.).
    if not out and err:
        out = err
    return {"ok": True, "tool": "tcp-connect", "host": host, "port": port,
            "ssl": ssl, "timeout": timeout, "output": _truncate(out)}

# ── Dispatch table (names here == MCPServer CR `tools:` entries) ───────────────
HANDLERS = {
    "probe-target":          probe_target,
    "enum-shares":           enum_shares,
    "shell-exploit":         shell_exploit,
    "submit-findings":       submit_findings,
    "flag-harvest":          flag_harvest,
    "engagement-state":      engagement_state,
    "masscan-scan":          masscan_scan,
    "rustscan-fast":         rustscan_fast,
    "subfinder-enum":        subfinder_enum,
    "httpx-probe":           httpx_probe,
    "amass-enum":            amass_enum,
    "feroxbuster":           feroxbuster,
    "whatweb-fingerprint":   whatweb_fingerprint,
    "nuclei-scan":           nuclei_scan,
    "ffuf-fuzz":             ffuf_fuzz,
    "sqlmap-inject":         sqlmap_inject,
    "nikto-scan":            nikto_scan,
    "wpscan-analyze":        wpscan_analyze,
    "commix-inject":         commix_inject,
    "xsstrike-scan":         xsstrike_scan,
    "searchsploit-lookup":   searchsploit_lookup,
    "fetch-exploit":         fetch_exploit,
    "run-exploit":           run_exploit,
    "msfvenom-payload":      msfvenom_payload,
    "metasploit-exploit":    metasploit_exploit,
    "hydra-brute":           hydra_brute,
    "john-crack":            john_crack,
    "netexec-scan":          netexec_scan,
    "rpcclient-enum":        rpcclient_enum,
    "smbmap-scan":           smbmap_scan,
    "bloodhound-collect":    bloodhound_collect,
    "trivy-scan":            trivy_scan,
    "checkov-iac":           checkov_iac,
    "gitleaks-scan":         gitleaks_scan,
    "volatility-analyze":    volatility_analyze,
    "yara-scan":             yara_scan,
    "binwalk-analyze":       binwalk_analyze,
    # Persistent session / raw-TCP tooling for interactive red-teaming.
    "tcp-connect":           tcp_connect,
    "tmux-new":              tmux_new,
    "tmux-send":             tmux_send,
    "tmux-recv":             tmux_recv,
    "tmux-kill":             tmux_kill,
}

# ── MCP JSON-RPC ───────────────────────────────────────────────────────────────

def handle_rpc(method, params, req_id):
    params = params or {}
    if method == "initialize":
        return {
            "protocolVersion": "2024-11-05",
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "pwnbox-mcp-server", "version": "1.1.0"},
        }, req_id
    if method == "notifications/initialized":
        return None, None
    if method == "tools/list":
        return {"tools": TOOLS}, req_id
    if method == "tools/call":
        name = params.get("name", "")
        args = params.get("arguments") or {}
        handler = HANDLERS.get(name)
        if not handler:
            return {"content": [{"type": "text", "text": f"unknown tool: {name}"}], "isError": True}, req_id
        # Validate args against the handler's real signature. The old code fell
        # back to `handler(args)` on TypeError, which passed the whole dict as
        # the first positional arg — a silent-corruption bug (e.g. feeding the
        # args dict into `target` for tmux_recv). Drop unknown/missing params
        # explicitly and surface the mismatch as a clear error instead.
        try:
            sig = inspect.signature(handler)
            bound = sig.bind_partial(**args)   # drops nothing silently; rejects unknown keys
            result = handler(*bound.args, **bound.kwargs)
        except TypeError as e:
            return {"content": [{"type": "text", "text": f"{name}: bad arguments: {e}"}], "isError": True}, req_id
        except Exception as e:  # noqa
            return {"content": [{"type": "text", "text": f"{type(e).__name__}: {e}"}], "isError": True}, req_id
        return {"content": [{"type": "text", "text": json.dumps(result)}], "isError": False}, req_id
    return {"error": {"code": -32601, "message": f"Method not found: {method}"}}, req_id

class MCPHandler(BaseHTTPRequestHandler):
    def log_message(self, *a): pass

    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200); self.end_headers()
            self._safe_write(b"ok")
        else:
            self.send_response(404); self.end_headers()

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        try:
            body = self.rfile.read(length)
        except (BrokenPipeError, ConnectionResetError):
            return
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._respond({"jsonrpc": "2.0", "id": None,
                           "error": {"code": -32700, "message": "Parse error"}})
            return
        result, resp_id = handle_rpc(req.get("method", ""), req.get("params"), req.get("id"))
        if result is None and resp_id is None:
            self.send_response(204); self.end_headers(); return
        if isinstance(result, dict) and "error" in result and len(result) == 1:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "error": result["error"]})
        else:
            self._respond({"jsonrpc": "2.0", "id": resp_id, "result": result})

    # Writes that swallow broken pipes: clients (K8s probes / model-router keep-alive)
    # sometimes close the socket before we finish writing, which would otherwise raise
    # BrokenPipeError and, under the single-threaded server, block probes during a scan.
    # With ThreadingHTTPServer a stuck handler can't starve /healthz anyway; this just
    # keeps the logs clean.
    def _safe_write(self, data):
        try:
            self.wfile.write(data)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _respond(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self._safe_write(body)

if __name__ == "__main__":
    print(f"pwnbox-mcp-server v1.1 listening on :{PORT} (threaded, tun0={_tun_up()})", flush=True)
    ThreadingHTTPServer(("", PORT), MCPHandler).serve_forever()
