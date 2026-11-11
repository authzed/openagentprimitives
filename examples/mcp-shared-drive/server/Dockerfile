# The unmodified MCP filesystem server behind a stdio-to-Streamable-HTTP bridge,
# with the fictional demo drive baked in.
#
# Built to OAP's SidecarToolbox contract: binds $MCP_PORT, serves MCP at /mcp
# and a health check at /healthz, runs as a numeric non-root user on a
# read-only root filesystem (only /tmp is writable), and needs no network at
# runtime. The before agent runs this same image with `docker run`.
FROM node:24-bookworm-slim@sha256:01d566e8d94d8c619591d78652babfccd3c6992a3dffa9ed5ab39e11cf6c70e9

WORKDIR /opt/app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --ignore-scripts && npm cache clean --force

COPY drive/ /opt/drive-seed/
# The server is told to serve /shared, which points into /tmp: the only place
# the sidecar may write. entrypoint.sh seeds it on start.
RUN ln -s /tmp/shared /shared
COPY --chmod=0755 entrypoint.sh /entrypoint.sh

ENV PATH=/opt/app/node_modules/.bin:$PATH
USER 10001
ENTRYPOINT ["/entrypoint.sh"]
