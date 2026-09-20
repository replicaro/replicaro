#!/usr/bin/env bash
set -euo pipefail

build_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$build_dir/../.." && pwd)
target=${1:?linux_amd64 or linux_arm64}
(cd "$root/backend" && go run ./tools/targetverify --target "$target" >&2)
release_commit=75849dce7cc37e4319b633df1f116ca895c71a12
case "$target" in
  linux_amd64)
    asset_id=456065460
    expected=1cc49bcf1e2ccd593c379adb17c9f85a36d619088296504de95b1d06215aebbf
    ;;
  linux_arm64)
    asset_id=456064894
    expected=7d5d772b7c32f0c84caf0a452a3072a5709027d7eac5856feb89a7a7a8881372
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
