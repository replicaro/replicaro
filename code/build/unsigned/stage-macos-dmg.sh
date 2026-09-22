#!/usr/bin/env bash
set -euo pipefail

[[ $# -eq 3 ]] || { echo "usage: stage-macos-dmg.sh create|replace-app APP OUTPUT" >&2; exit 2; }
mode=$1
app=$2
output=$3
build_dir=$(cd "$(dirname "$0")" && pwd)
layout="$build_dir/macos-dmg-layout"
finder_template="$layout/FinderLayout.dsstore"
background="$layout/installer-background.png"

[[ -d "$app" && ! -L "$app" ]] || { echo "macOS DMG app is unavailable or redirected" >&2; exit 2; }
[[ -f "$finder_template" && ! -L "$finder_template" ]] || exit 2
[[ -f "$background" && ! -L "$background" ]] || exit 2
printf '%s  %s\n' 778323ee3e417497f48d2a48329fb19b6ec0efb11a772417be0f77b5b74f58a4 "$finder_template" | shasum -a 256 -c - >/dev/null
printf '%s  %s\n' f215ce1c3b04d171db3ee66764d31dee929bd1192fa600cbca4919e2e56ed1f9 "$background" | shasum -a 256 -c - >/dev/null
python3 "$layout/render-installer-background.py" "--check=$background"

case "$mode" in
  create)
    [[ -d "$output" && ! -L "$output" && -z "$(find "$output" -mindepth 1 -maxdepth 1 -print -quit)" ]] || {
      echo "macOS DMG staging directory must be an empty ordinary directory" >&2; exit 2;
    }
    mkdir "$output/.background"
    # The source filename is intentionally not .DS_Store: ordinary source
    # handoff must include it even where platform ignore rules hide Finder
    # metadata. Only the staged DMG receives Finder's exact native name.
    cp "$finder_template" "$output/.DS_Store"
    cp "$background" "$output/.background/installer-background.png"
    # ditto preserves signed bundle metadata and resource forks. The checked-in
    # layout bytes are copied separately so Finder presentation never depends
    # on interactive Finder or AppleScript state.
    ditto "$app" "$output/Replicaro.app"
    ln -s /Applications "$output/Applications"
    ;;
  replace-app)
    expected=$'.DS_Store\n.background\nApplications\nReplicaro.app'
    [[ "$(find "$output" -mindepth 1 -maxdepth 1 -exec basename {} \; | LC_ALL=C sort)" == "$expected" ]] || {
      echo "macOS DMG staging inventory changed before signed app replacement" >&2; exit 2;
    }
    [[ -L "$output/Applications" && "$(readlink "$output/Applications")" == /Applications ]] || exit 2
    [[ -d "$output/.background" && ! -L "$output/.background" ]] || exit 2
    [[ "$(find "$output/.background" -mindepth 1 -maxdepth 1 -exec basename {} \;)" == installer-background.png ]] || exit 2
    [[ -f "$output/.DS_Store" && ! -L "$output/.DS_Store" &&
       -f "$output/.background/installer-background.png" && ! -L "$output/.background/installer-background.png" &&
       -d "$output/Replicaro.app" && ! -L "$output/Replicaro.app" ]] || exit 2
    cmp "$finder_template" "$output/.DS_Store"
    cmp "$background" "$output/.background/installer-background.png"
    rm -R "$output/Replicaro.app"
    ditto "$app" "$output/Replicaro.app"
    ;;
  *) echo "macOS DMG staging mode is invalid" >&2; exit 2 ;;
esac
