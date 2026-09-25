#!/usr/bin/env bash
# Reclaim disk on the ubuntu-latest runner before a heavy Go build.
#
# ubuntu-latest ships ~14 GB free on /, which the -race build of the whole
# module (plus a fresh module download and the envtest control-plane binaries)
# exhausts mid-run — the runner worker itself dies with "No space left on
# device", which reads as a mysterious crash rather than a test failure. We
# delete the large preinstalled toolchains this repo never uses, reclaiming
# ~25 GB. Done in inline shell rather than a third-party action to keep the CI
# supply surface small (see the OSS hardening pass). Never touch
# /opt/hostedtoolcache/go — setup-go serves the toolchain from there.
set -euo pipefail

echo "Disk before cleanup:"
df -h /

sudo rm -rf \
  /usr/local/lib/android \
  /usr/share/dotnet \
  /opt/ghc \
  /usr/local/.ghcup \
  /opt/hostedtoolcache/CodeQL \
  /usr/share/swift \
  /usr/local/share/powershell \
  /usr/lib/jvm || true

sudo docker image prune --all --force || true

echo "Disk after cleanup:"
df -h /
