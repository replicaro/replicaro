#!/usr/bin/env bash
set -euo pipefail

build_dir=$(cd "$(dirname "$0")" && pwd)
target=${1:?target is required}
field() { (cd "$build_dir" && go run ./cmd/contract inputs --file "$build_dir/inputs.json" --field "$1"); }

[[ "$(go env GOVERSION)" == "go$(field go)" ]] || { echo "exact Go $(field go) is required" >&2; exit 2; }
[[ "$(node --version)" == "v$(field node)" ]] || { echo "exact Node $(field node) is required" >&2; exit 2; }
[[ "$(npm --version)" == "$(field npm)" ]] || { echo "exact npm $(field npm) is required" >&2; exit 2; }

case "$target" in
  linux_amd64) [[ "$(uname -s)/$(uname -m)" == Linux/x86_64 ]] ;;
  linux_arm64) [[ "$(uname -s)/$(uname -m)" == Linux/aarch64 ]] ;;
  darwin_amd64) [[ "$(uname -s)/$(uname -m)" == Darwin/x86_64 ]] ;;
  darwin_arm64) [[ "$(uname -s)/$(uname -m)" == Darwin/arm64 ]] ;;
  *) echo "unsupported Unix target: $target" >&2; exit 2 ;;
esac || { echo "$target requires its exact native runner" >&2; exit 2; }
