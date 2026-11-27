#!/bin/sh
# Seed the demo drive into /tmp on first start, then serve the filesystem
# server over Streamable HTTP.
#
# The bridge runs stateless: it starts and initializes a fresh server process
# for each request, and the drive's files persist on disk between them. In
# stateful mode the server asks each client for its MCP Roots right after the
# handshake and holds every tool call until the answer arrives; when that
# request reaches the bridge before the client has opened its event stream it
# is dropped, and tool calls hang for 60 seconds. Stateless mode never forwards
# the client's Roots capability, so the server never asks.
#
# The bridge's info log goes to stdout and copies every message verbatim,
# file contents included; that would put payroll rows in the container log,
# outside the permission checks this demo is about. stdout is discarded. Its
# errors and the server's own stderr go to stderr, which is kept.
set -eu

if [ ! -d /tmp/shared ]; then
  cp -R /opt/drive-seed /tmp/shared
fi

exec supergateway \
  --stdio "mcp-server-filesystem /shared" \
  --outputTransport streamableHttp \
  --port "${MCP_PORT:-8000}" \
  --streamableHttpPath /mcp \
  --healthEndpoint /healthz >/dev/null
