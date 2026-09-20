#!/usr/bin/env bash
set -euo pipefail

build_dir=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$build_dir/../.." && pwd)
target=${1:?linux_amd64 or linux_arm64}
(cd "$root/backend" && go run ./tools/targetverify --target "$target" >&2)
version=1.9.1
case "$target" in
  linux_amd64)
    filename=appimagetool-x86_64.AppImage
    url=https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-x86_64.AppImage
    expected=ed4ce84f0d9caff66f50bcca6ff6f35aae54ce8135408b3fa33abfc3cb384eb0
    ;;
  linux_arm64)
    filename=appimagetool-aarch64.AppImage
    url=https://github.com/AppImage/appimagetool/releases/download/1.9.1/appimagetool-aarch64.AppImage
    expected=f0837e7448a0c1e4e650a93bb3e85802546e60654ef287576f46c71c126a9158
    ;;
  *) echo "target must be linux_amd64 or linux_arm64" >&2; exit 2 ;;
esac

tools_dir="${REPLICARO_TARGET_OUT:?target-contained output root is required}/tools"
tool="$tools_dir/appimagetool-$version-$target.AppImage"
mkdir -p "$tools_dir"
if [[ -f "$tool" ]] && [[ "$(sha256sum "$tool" | awk '{print $1}')" == "$expected" ]]; then
  chmod 700 "$tool"
  printf '%s\n' "$tool"
  exit 0
fi
if [[ -e "$tool" ]]; then
  unlink "$tool"
fi
temporary=$(mktemp "$tools_dir/.${filename}.XXXXXX")
cleanup() { [[ ! -e "$temporary" ]] || unlink "$temporary"; }
trap cleanup EXIT
curl --fail --location --proto '=https' --proto-redir '=https' --max-redirs 5 \
  --retry 5 --retry-delay 5 --retry-all-errors --retry-max-time 60 \
  --output "$temporary" "$url"
actual=$(sha256sum "$temporary" | awk '{print $1}')
if [[ "$actual" != "$expected" ]]; then
  echo "appimagetool checksum mismatch: $actual" >&2
  exit 1
fi
chmod 700 "$temporary"
mv "$temporary" "$tool"
trap - EXIT
printf '%s\n' "$tool"
