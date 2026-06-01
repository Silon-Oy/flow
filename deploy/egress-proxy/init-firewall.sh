#!/usr/bin/env bash
# init-firewall.sh — default-deny egress allow-list for the runner-host egress
# proxy (§11.2). Adapted from Anthropic's Claude Code devcontainer
# init-firewall.sh (ipset + iptables default-deny).
#
# The per-run orchestrator containers are attached ONLY to the flow-egress
# network; this proxy is the single route out. Everything is denied except the
# hosts on the allow-list. Denied connections are a circumvention signal and are
# logged (§11.6) — see the proxy's access log.
#
# Run inside the egress-proxy container at startup, before the proxy serves.
set -euo pipefail

# Allow-list (§11.2). Keep narrow: every entry is an explicit trust decision.
ALLOWED_DOMAINS=(
  "github.com"
  "api.github.com"
  "codeload.github.com"        # github archive/tarball downloads
  "objects.githubusercontent.com" # release assets / LFS
  "registry.npmjs.org"
  "repo.packagist.org"
  "api.anthropic.com"
)

ipset create allowed-egress hash:ip -exist

resolve_into_ipset() {
  local domain="$1"
  local ips
  ips=$(getent ahostsv4 "$domain" | awk '{print $1}' | sort -u) || true
  if [ -z "$ips" ]; then
    echo "WARN: could not resolve $domain — skipping (will be denied)" >&2
    return 0
  fi
  while IFS= read -r ip; do
    [ -n "$ip" ] || continue
    ipset add allowed-egress "$ip" -exist
  done <<<"$ips"
}

for d in "${ALLOWED_DOMAINS[@]}"; do
  resolve_into_ipset "$d"
done

# Flush existing rules in the OUTPUT chain we manage.
iptables -F OUTPUT || true

# Always allow loopback and established/related return traffic.
iptables -A OUTPUT -o lo -j ACCEPT
iptables -A OUTPUT -m state --state ESTABLISHED,RELATED -j ACCEPT

# Allow DNS so domain resolution keeps working (resolution itself is the proxy's
# job; the orchestrator container uses the proxy, not direct DNS).
iptables -A OUTPUT -p udp --dport 53 -j ACCEPT
iptables -A OUTPUT -p tcp --dport 53 -j ACCEPT

# Allow egress only to the resolved allow-list IPs.
iptables -A OUTPUT -m set --match-set allowed-egress dst -j ACCEPT

# Default deny everything else, with a log target for §11.6 visibility.
iptables -A OUTPUT -j LOG --log-prefix "FLOW-EGRESS-DENIED: " --log-level 4
iptables -A OUTPUT -j REJECT

echo "init-firewall: default-deny egress active; ${#ALLOWED_DOMAINS[@]} domains allowed"
