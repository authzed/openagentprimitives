# ap-toolchain-node — a relocation-free Node.js toolchain overlay.
#
# The payload is built at /opt/ap-toolchains/node and mounted at
# /opt/ap-toolchains/node. Build path == mount path, so nothing inside the
# payload (npm/npx/pnpm shebangs, node's own module resolution) needs
# rewriting. Do NOT change either path independently.
#
# The builder stage MUST stay bookworm-based: `node` is dynamically linked and
# resolves libc/libstdc++ from the *sandbox* rootfs (debian 12, see
# Dockerfile.sandbox), not from this image. A newer glibc here produces a
# `GLIBC_2.xx not found` at exec time, far from its cause.
#
# Pinned digest. Refresh with:
#   docker pull node:22-bookworm
#   docker image inspect node:22-bookworm --format '{{index .RepoDigests 0}}'
FROM node@sha256:a25c9934ff6382cd4f08b6bc26c82bf4ea69b1e6f8dabfb2ead457374127c365 AS build

ENV TC=/opt/ap-toolchains/node

# The official image installs node/npm/npx/corepack under /usr/local. Copy the
# whole prefix rather than cherry-picking: npm resolves its own lib/ relative to
# the binary, so a partial copy breaks it in non-obvious ways.
RUN mkdir -p "${TC}" && cp -a /usr/local/. "${TC}/"

# The node image leaves three symlinks pointing OUTSIDE /usr/local, which the
# copy above cannot follow:
#   bin/yarn, bin/yarnpkg -> /opt/yarn-v1.22.22/...   (yarn lives outside the prefix)
#   bin/nodejs            -> /usr/local/bin/node      (absolute, not relative)
# All three dangle once the payload is mounted at /opt/ap-toolchains/node. The
# `nodejs` one is the dangerous shape: it is an absolute path into the SANDBOX
# rootfs, so the day the sandbox base image ships a node, `nodejs` would silently
# resolve to that interpreter instead of this overlay's.
#
# Bring yarn inside the prefix and repoint every link relatively.
RUN cp -a /opt/yarn-v1.22.22 "${TC}/yarn" \
 && ln -sfn ../yarn/bin/yarn    "${TC}/bin/yarn" \
 && ln -sfn ../yarn/bin/yarnpkg "${TC}/bin/yarnpkg" \
 && ln -sfn node                "${TC}/bin/nodejs"

ENV PATH="${TC}/bin:${PATH}"

# Versions are pinned, never floating. `npm install -g typescript` resolves the
# `latest` dist-tag, which is currently a major ahead (7.x) of what real repos
# pin; `pnpm` likewise floats. A toolchain overlay that silently changes major
# versions between rebuilds is worse than one that is slightly stale.
#
# PNPM_VERSION must match the consuming repo's package.json "packageManager".
# TS_VERSION tracks the 5.x line that repos pin today; it backs the language
# server only — a repo with its own typescript devDependency still resolves that
# one first, out of its node_modules.
ARG PNPM_VERSION=11.1.1
ARG TS_VERSION=5.9.2
ARG TS_LANGUAGE_SERVER_VERSION=5.3.0

# pnpm is installed as a REAL binary, not via corepack. Corepack ships only a
# shim: `corepack prepare pnpm@x --activate` caches the tarball under
# $COREPACK_HOME (the *builder's* HOME), not under ${TC} — so the payload would
# carry a shim with no package behind it, and the first `pnpm` call in a session
# would try to DOWNLOAD from registry.npmjs.org. The sandbox's egress is
# restricted, so that is a hang or a proxy error rather than a clear message.
# Installing the real package puts pnpm in ${TC}/bin with nothing to fetch.
#
# typescript + typescript-language-server are npm-only — neither publishes a
# standalone binary — which is why they are installed here rather than fetched
# as release assets.
RUN npm install -g --prefix "${TC}" \
      "pnpm@${PNPM_VERSION}" \
      "typescript@${TS_VERSION}" \
      "typescript-language-server@${TS_LANGUAGE_SERVER_VERSION}"

# Self-verify, so a broken toolchain fails the build rather than a session.
# The pnpm check is an EQUALITY check on purpose: if corepack ever shadows the
# real binary again, or the pin drifts, `pnpm -v` reports a different version
# and the build fails here instead of an agent's `pnpm install` failing offline.
RUN "${TC}/bin/node" --version \
 && "${TC}/bin/npm" --version \
 && "${TC}/bin/typescript-language-server" --version \
 && got="$("${TC}/bin/pnpm" --version)" \
 && { [ "${got}" = "${PNPM_VERSION}" ] || { echo "pnpm is ${got}, want ${PNPM_VERSION}"; exit 1; }; } \
 && got="$("${TC}/bin/tsc" --version | awk '{print $2}')" \
 && { [ "${got}" = "${TS_VERSION}" ] || { echo "tsc is ${got}, want ${TS_VERSION}"; exit 1; }; } \
 && "${TC}/bin/yarn" --version >/dev/null

# No symlink in the payload may be absolute or dangling. An absolute link escapes
# the overlay and resolves against the SANDBOX rootfs — silently binding to a
# different binary, or to nothing. A dangling link means `provides:` on the CR is
# a false claim. Enforced at build time because neither failure is visible until
# an agent runs the tool.
RUN cd "${TC}" && bad=""; \
    for l in $(find . -type l); do \
      t="$(readlink "$l")"; \
      case "$t" in /*) bad="${bad}  ${l} -> ${t} (absolute)\n";; esac; \
      [ -e "$l" ] || bad="${bad}  ${l} -> ${t} (dangling)\n"; \
    done; \
    if [ -n "$bad" ]; then printf 'payload has escaping/dangling symlinks:\n%b' "$bad"; exit 1; fi; \
    echo "symlink audit: all $(find . -type l | wc -l | tr -d ' ') links are relative and resolve"

# World-readable/executable: the init container copies as uid 1000 and cannot
# chown, and native image volumes (the future delivery kind) ignore fsGroup and
# keep in-image ownership.
RUN chmod -R a+rX "${TC}"

# busybox, not scratch: the `image` delivery kind's init container needs
# /bin/sh, mkdir, and cp. musl-static, ~1.4MB, and never executed inside the
# sandbox — only as the init container.
#
# Pinned digest. Refresh with:
#   docker pull busybox:1.37-musl
#   docker image inspect busybox:1.37-musl --format '{{index .RepoDigests 0}}'
FROM busybox@sha256:222ad6d973c0d198014546a65cd02c5fdedcc172123c5b4c2bf0af636550bd94
COPY --from=build --chown=1000:1000 /opt/ap-toolchains/node /opt/ap-toolchains/node
