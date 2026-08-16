#!/usr/bin/env bash
# pwnbox entrypoint (runs in the privileged red-pwnbox pod):
#   1. best-effort bring up the HTB OpenVPN tunnel on tun0 (RETRY; MUST NOT crash the
#      pod — a bad/empty .ovpn or transient DNS must not take the container down, or
#      skaffold's readiness check will uninstall the whole release and GC the KB pods),
#   2. ALWAYS serve the MCP toolbelt (pwnbox-mcp-server.py) over HTTP.
# The agent lives in a SEPARATE, restricted pod (red-commander) and calls these tools
# via the model-router; if tun0 is down, the tools report "tun0 not up" instead of
# the container dying.
#
# The pod is privileged (pod spec: privileged=true + /dev/net/tun hostPath) so openvpn
# can create the tun device and configure routes.
set -uo pipefail   # NOTE: no -e — openvpn must never take the container down.

PVLINE="${PORT:-8080}"
OVPN_DIR="/etc/htb"
OVPN_FILE="${OVPN_DIR}/${OVPN_FILE_NAME:-htbea.ovpn}"

if [ ! -f "$OVPN_FILE" ]; then
  echo "[pwnbox] ERROR: HTB OpenVPN config not found at $OVPN_FILE — tun0 will be down." >&2
  echo "[pwnbox] Ensure the htb-ovpn Secret is mounted at /etc/htb (secretRef name=htb-ovpn, mountPath=/etc/htb)." >&2
elif [ ! -s "$OVPN_FILE" ]; then
  echo "[pwnbox] ERROR: $OVPN_FILE exists but is EMPTY — check the AGENT_ORC_HTB_OVPN_CONFIG value / htb-ovpn Secret." >&2
fi

try_openvpn() {
  # Kill any stale openvpn first so a retry isn't blocked by an old PID.
  pkill -x openvpn 2>/dev/null || true
  rm -f /tmp/openvpn.pid
  if openvpn --config "$OVPN_FILE" --daemon --writepid /tmp/openvpn.pid --log /tmp/openvpn.log; then
    for _ in $(seq 1 15); do
      [ -d /sys/class/net/tun0 ] && return 0
      sleep 1
    done
  fi
  return 1
}

if [ -f "$OVPN_FILE" ] && [ -s "$OVPN_FILE" ]; then
  for attempt in 1 2 3; do
    echo "[pwnbox] Starting OpenVPN (attempt $attempt) from $OVPN_FILE ..."
    if try_openvpn; then
      echo "[pwnbox] tun0 is up."
      break
    fi
    echo "[pwnbox] WARNING: tun0 not up after attempt $attempt. openvpn log tail:" >&2
    tail -n 30 /tmp/openvpn.log >&2 2>/dev/null || true
    [ "$attempt" -lt 3 ] && sleep 3
  done
fi

if [ -d /sys/class/net/tun0 ]; then
  echo "[pwnbox] Tunnel established; starting MCP toolbelt server."
else
  echo "[pwnbox] WARNING: tun0 is NOT up — serving MCP in offline mode." >&2
  echo "[pwnbox] Network tools will report 'tun0 not up'. Inspect with: kubectl logs deploy/red-pwnbox -c pwnbox" >&2
  echo "[pwnbox] openvpn log:" >&2
  tail -n 30 /tmp/openvpn.log >&2 2>/dev/null || true
fi

# Seed the exploit sandbox for fetch-exploit / run-exploit.
#
# By default this is EPHEMERAL: wiped on every pod start so no payload from a
# previous engagement survives a restart (security isolation). BUT when the
# operator mounts a persistent workspace PVC at EXPLOIT_DIR (pwnbox.exploitWorkspace
# enabled) the dir IS a mount point and `rm -rf`-ing it fails with "Device or
# resource busy" — and, more importantly, persistence is the whole point of the PVC.
# Detect the mount: on a PVC, keep contents across restarts (just ensure the dir
# exists); on the ephemeral writable layer, wipe+recreate as before.
EXPLOIT_DIR="${EXPLOIT_DIR:-/tmp/exploits}"
is_mount_point() {
  if command -v findmnt >/dev/null 2>&1; then
    findmnt --noheadings --target "$1" >/dev/null 2>&1
  else
    # /proc/self/mountinfo field 5 == mount point.
    awk -v p="$1" '($5==p){found=1} END{exit !found}' /proc/self/mountinfo 2>/dev/null
  fi
}
if is_mount_point "$EXPLOIT_DIR"; then
  # Persistent volume: do NOT wipe — that is the persistence contract.
  mkdir -p "$EXPLOIT_DIR" && chmod 1777 "$EXPLOIT_DIR"
else
  # Ephemeral container writable layer: wipe stale payloads for isolation.
  rm -rf "$EXPLOIT_DIR" && mkdir -p "$EXPLOIT_DIR" && chmod 1777 "$EXPLOIT_DIR"
fi

echo "[pwnbox] Starting MCP toolbelt server on :${PVLINE} (http://localhost:8080 to the model-router sidecar)"
PYTHON="$(command -v python3 || command -v python)"
exec "$PYTHON" /app/pwnbox-mcp-server.py
