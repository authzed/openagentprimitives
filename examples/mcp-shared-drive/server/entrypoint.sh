#!/bin/sh
# Seed the demo drive into /tmp on first start, then serve the filesystem
# server over stateful Streamable HTTP. Stateful mode issues Mcp-Session-Id on
# initialize, which OAP's MCP client expects.
set -eu

if [ ! -d /tmp/shared ]; then
  cp -R /opt/drive-seed /tmp/shared
fi

exec supergateway \
  --stdio "mcp-server-filesystem /shared" \
  --outputTransport streamableHttp \
  --stateful \
  --port "${MCP_PORT:-8000}" \
  --streamableHttpPath /mcp \
  --healthEndpoint /healthz
