#!/usr/bin/env bash
# Package, push, sign and verify every example agent bundle under examples/.
# Run by .github/workflows/publish-agents.yaml; runnable by hand with the same
# environment.
#
# Required env:
#   OAP                    path to the oap binary
#   AGENTS_REPO            OCI repository prefix, e.g. ghcr.io/authzed/openagentprimitives/agents
#   TAG                    immutable tag for this publish (e.g. the short commit sha)
#   SIGNING_KEY_FILE       PEM ECDSA private key used by `oap agent sign`
#   VERIFY_KEY_FILE        matching PEM public key; every push is verified against it
# Optional:
#   OAP_REGISTRY_USERNAME / OAP_REGISTRY_PASSWORD / OAP_REGISTRY_HOST
#                          push credentials and the registry host they are for
#                          (all three, or none for an anonymous local registry);
#                          OAP_REGISTRY_HOST must be AGENTS_REPO's host
#   PLAIN_HTTP=1           talk plain HTTP to the registry (local dry runs only)
#
# Each bundle is pushed as <AGENTS_REPO>/<agent-name>:<TAG>, where <agent-name>
# is the name the loader derives (it can differ from the folder name), signed
# by digest, and verified anonymously by digest so a package that isn't public
# fails here rather than in a user's install. Only then is :latest moved to that
# digest: a failed sign or verify leaves :latest on its last good bundle.
set -euo pipefail

: "${OAP:?}" "${AGENTS_REPO:?}" "${TAG:?}" "${SIGNING_KEY_FILE:?}" "${VERIFY_KEY_FILE:?}"

# oap itself enforces all-or-none for the three credential vars and refuses a
# push whose ref is on a host other than OAP_REGISTRY_HOST; checking up front
# fails before anything is packaged, naming both values.
# Hosts compare case-insensitively, as oap compares them; tr rather than
# ${var,,} so the script still runs on macOS's bash 3.2.
lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }
registry_host=${AGENTS_REPO%%/*}
if [ -n "${OAP_REGISTRY_HOST:-}" ] && [ "$(lower "$OAP_REGISTRY_HOST")" != "$(lower "$registry_host")" ]; then
  echo "OAP_REGISTRY_HOST ($OAP_REGISTRY_HOST) is not AGENTS_REPO's registry host ($registry_host)" >&2
  exit 1
fi

repo_root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# Root bundles only: an oap.yaml under another bundle's dependencies/ is
# embedded into its parent by `oap agent package`, not published on its own.
bundles=()
while IFS= read -r dir; do bundles+=("$dir"); done < <(cd "$repo_root" && find examples -name oap.yaml -not -path '*/dependencies/*' -exec dirname {} \; | sort)
plain=()
if [ -n "${PLAIN_HTTP:-}" ]; then plain=(--plain-http); fi
if [ "${#bundles[@]}" -eq 0 ]; then
  echo "no example bundles found under examples/" >&2
  exit 1
fi

for dir in "${bundles[@]}"; do
  out="$work/$(echo "$dir" | tr '/' '_')"
  mkdir -p "$out"
  # package writes <agent-name>.oap into the current directory; that file name
  # is how we learn the derived name without parsing human-readable output.
  (cd "$out" && "$OAP" agent package "$repo_root/$dir" >/dev/null)
  shopt -s nullglob
  files=("$out"/*.oap)
  shopt -u nullglob
  if [ "${#files[@]}" -ne 1 ]; then
    echo "$dir: expected exactly one .oap from oap agent package, got ${#files[@]}" >&2
    exit 1
  fi
  file=${files[0]}
  name=$(basename "$file" .oap)
  ref="$AGENTS_REPO/$name"

  echo "::group::$dir -> $ref"
  digest=$("$OAP" agent push ${plain[@]+"${plain[@]}"} "$ref:$TAG" "$file")
  "$OAP" agent sign ${plain[@]+"${plain[@]}"} "$ref@$digest" --key "$SIGNING_KEY_FILE"
  OAP_REGISTRY_USERNAME='' OAP_REGISTRY_PASSWORD='' OAP_REGISTRY_HOST='' \
    "$OAP" agent verify ${plain[@]+"${plain[@]}"} "$ref@$digest" --key "$VERIFY_KEY_FILE"
  # Verified; only now may :latest move. Re-pushing the same file yields the
  # same manifest, so the digest must match what was just signed.
  latest_digest=$("$OAP" agent push ${plain[@]+"${plain[@]}"} "$ref:latest" "$file")
  if [ "$digest" != "$latest_digest" ]; then
    echo "$ref: :latest ($latest_digest) is not the signed digest ($digest)" >&2
    exit 1
  fi
  echo "::endgroup::"
done
