#!/usr/bin/env bash
# Stages the aigen model and llama-server for a `-tags aigen` build of the host.
# Usage: scripts/ai-bundle.sh <dir with llama-server/ and model.gguf>
#   e.g. scripts/ai-bundle.sh ~/Library/Caches/aigen/<version>   (what `aigen` unpacks)
set -euo pipefail

src="${1:?usage: scripts/ai-bundle.sh <dir with llama-server/ and model.gguf>}"
out="$(cd "$(dirname "$0")/.." && pwd)/internal/ai/assets"
[[ -x "$src/llama-server/llama-server" && -f "$src/model.gguf" ]] || {
  echo "ai-bundle: $src needs llama-server/llama-server and model.gguf" >&2
  exit 1
}

rm -rf "$out" && mkdir -p "$out"
# No AppleDouble (._*) files, which macOS tar adds for extended attributes.
COPYFILE_DISABLE=1 tar -czf "$out/llama-server.tar.gz" --exclude '._*' -C "$src" llama-server
cp "$src/model.gguf" "$out/model.gguf"
# The version names the unpack folder, so a new model or server unpacks fresh.
cat "$out/model.gguf" "$out/llama-server.tar.gz" | shasum -a 256 | cut -c1-16 > "$out/version"

du -sh "$out"/* | sed "s|$out/||"
echo "staged; build with: go build -tags aigen ./repogo"
