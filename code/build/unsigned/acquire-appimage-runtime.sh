#!/usr/bin/env bash
set -euo pipefail

build_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$build_dir/../.." && pwd)
target=${1:?linux_amd64 or linux_arm64}
(cd "$root/backend" && go run ./tools/targetverify --target "$target" >&2)
release_commit=8f39b89e2ac31e1640b3d3f7e9a5108e6ce805fa
case "$target" in
  linux_amd64)
    asset_id=596078161
    expected=156f4bdbde9c52d01814600013e0a273f0118dc2de98975f3c8c63427ec79074
    ;;
  linux_arm64)
    asset_id=596077442
    expected=b4ff0030242d0c3bb12ce40541828303cf167493f4793456f0436edd6255c39d
    ;;
  *) echo "target must be linux_amd64 or linux_arm64" >&2; exit 2 ;;
esac

url="https://api.github.com/repos/AppImage/type2-runtime/releases/assets/$asset_id"
tools_dir="${REPLICARO_TARGET_OUT:?target-contained output root is required}/tools"
runtime="$tools_dir/appimage-runtime-$release_commit-$target"
mkdir -p "$tools_dir"
if [[ -f "$runtime" ]] && [[ "$(sha256sum "$runtime" | awk '{print $1}')" == "$expected" ]]; then
  chmod 600 "$runtime"
  printf '%s\n' "$runtime"
  exit 0
fi
if [[ -e "$runtime" ]]; then
  unlink "$runtime"
fi
temporary=$(mktemp "$tools_dir/.appimage-runtime.XXXXXX")
cleanup() { [[ ! -e "$temporary" ]] || unlink "$temporary"; }
trap cleanup EXIT
curl --fail --location --proto '=https' --proto-redir '=https' --max-redirs 5 \
  -H 'Accept: application/octet-stream' -H 'X-GitHub-Api-Version: 2022-11-28' \
  --output "$temporary" "$url"
actual=$(sha256sum "$temporary" | awk '{print $1}')
if [[ "$actual" != "$expected" ]]; then
  echo "AppImage runtime checksum mismatch: $actual" >&2
  exit 1
fi
chmod 600 "$temporary"
mv "$temporary" "$runtime"
trap - EXIT
printf '%s\n' "$runtime"
