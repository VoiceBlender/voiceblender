#!/usr/bin/env bash
#
# vb-firewall.sh — apply VoiceBlender's Docker access-control rules.
#
# Restricts the SIP / VSI / REST endpoints to an allowlist of source IPs while
# leaving RTP/media and container-to-container traffic open. Rules live in the
# VB-FILTER chain, hung off Docker's DOCKER-USER chain, so they survive container
# restart/recreate and Docker daemon restart. Pair with vb-firewall.service for
# host-reboot persistence.
#
# Usage:
#   sudo ./vb-firewall.sh apply     # (re)load rules from the .rules file
#   sudo ./vb-firewall.sh status    # show the active VB-FILTER chain
#   sudo ./vb-firewall.sh remove    # tear down VB-FILTER and its jump
#
# Configuration (env vars):
#   VB_EXT_IF      external interface(s), space-separated. Auto-detected from the
#                  default route if unset. Also settable with --iface "<if...>".
#   VB_RULES_FILE  path to the .rules file (default: alongside this script).

set -euo pipefail

CHAIN="VB-FILTER"
PARENT="DOCKER-USER"
TOKEN="__EXT_IF__"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RULES_FILE="${VB_RULES_FILE:-$SCRIPT_DIR/voiceblender-docker-user.rules}"

die() { echo "vb-firewall: $*" >&2; exit 1; }

require_root() {
  [ "$(id -u)" -eq 0 ] || die "must run as root (use sudo)."
}

require_docker_chain() {
  iptables -n -L "$PARENT" >/dev/null 2>&1 || die \
    "chain $PARENT not found — is Docker running? Docker creates it on start."
}

detect_ifaces() {
  ip -o route show default 2>/dev/null \
    | awk '{for (i=1;i<=NF;i++) if ($i=="dev") print $(i+1)}' \
    | sort -u
}

# Build a single iptables-restore stream: the -A VB-FILTER lines from the rules
# file, replicated once per external interface with the token substituted.
build_stream() {
  local ifaces="$1" ifc
  echo "*filter"
  echo ":$CHAIN - [0:0]"
  for ifc in $ifaces; do
    grep -E "^-A $CHAIN" "$RULES_FILE" | sed "s/$TOKEN/$ifc/g"
  done
  echo "COMMIT"
}

cmd_apply() {
  require_root
  require_docker_chain
  [ -f "$RULES_FILE" ] || die "rules file not found: $RULES_FILE"

  grep -qE "^-A $CHAIN" "$RULES_FILE" || die "no '-A $CHAIN' rules in $RULES_FILE — nothing to apply."

  local ifaces="${VB_EXT_IF:-$(detect_ifaces)}"
  [ -n "$ifaces" ] || die "could not detect external interface; set VB_EXT_IF or --iface."

  # Ensure the chain exists and is jumped to from DOCKER-USER exactly once.
  iptables -N "$CHAIN" 2>/dev/null || true
  iptables -C "$PARENT" -j "$CHAIN" 2>/dev/null || iptables -I "$PARENT" 1 -j "$CHAIN"

  iptables -F "$CHAIN"
  build_stream "$ifaces" | iptables-restore --noflush

  echo "vb-firewall: applied on interface(s): $ifaces"
  echo "vb-firewall: protected ports — tcp/8080 (REST+VSI), udp/5060 (SIP), tcp/5061 (SIP-TLS)"
}

cmd_status() {
  require_root
  if iptables -C "$PARENT" -j "$CHAIN" 2>/dev/null; then
    echo "jump: $PARENT -> $CHAIN [present]"
  else
    echo "jump: $PARENT -> $CHAIN [MISSING]"
  fi
  echo
  iptables -L "$CHAIN" -n -v --line-numbers 2>/dev/null || echo "chain $CHAIN does not exist"
}

cmd_remove() {
  require_root
  while iptables -C "$PARENT" -j "$CHAIN" 2>/dev/null; do
    iptables -D "$PARENT" -j "$CHAIN"
  done
  iptables -F "$CHAIN" 2>/dev/null || true
  iptables -X "$CHAIN" 2>/dev/null || true
  echo "vb-firewall: removed $CHAIN and its jump from $PARENT"
}

main() {
  local action="${1:-}"
  shift || true
  while [ $# -gt 0 ]; do
    case "$1" in
      --iface) VB_EXT_IF="${2:-}"; shift 2 ;;
      --iface=*) VB_EXT_IF="${1#*=}"; shift ;;
      *) die "unknown argument: $1" ;;
    esac
  done

  case "$action" in
    apply)  cmd_apply ;;
    status) cmd_status ;;
    remove) cmd_remove ;;
    *) cat >&2 <<EOF
usage: $0 {apply|status|remove} [--iface "<if> [<if>...]"]

  apply   (re)load rules from $RULES_FILE into the $CHAIN chain
  status  show the active $CHAIN chain and its jump from $PARENT
  remove  delete $CHAIN and its jump from $PARENT
EOF
       exit 2 ;;
  esac
}

main "$@"
