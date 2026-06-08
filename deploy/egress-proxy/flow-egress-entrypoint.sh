#!/bin/bash
# flow-egress-entrypoint.sh — wraps the stock ubuntu/squid entrypoint.
#
# The squid image's own entrypoint expects `command` to be plain squid args and
# has no iptables/ipset, so it cannot run the §11.2 default-deny firewall. This
# wrapper runs init-firewall.sh first (needs NET_ADMIN), then hands off to the
# image's original entrypoint (`entrypoint.sh`, still on PATH) with the squid
# args so log-forwarding to stdout and cache-dir setup are preserved.
set -euo pipefail

/usr/local/bin/init-firewall.sh

# Defer to the stock squid entrypoint with whatever squid args were passed
# (see Dockerfile.egress CMD). `entrypoint.sh` resolves via PATH to the image's
# /usr/local/bin/entrypoint.sh — NOT this file (different name avoids recursion).
exec entrypoint.sh "$@"
