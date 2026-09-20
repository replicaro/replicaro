#!/bin/sh
set -eu

bundle_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
: "${HOME:?HOME must be set}"
install_dir=${REPLICARO_INSTALL_DIR:-"$HOME/.local/opt/replicaro"}
ownership_marker=".replicaro-owned"
ownership_marker_value="replicaro-managed-v1"
data_home=${XDG_DATA_HOME:-"$HOME/.local/share"}
config_home=${XDG_CONFIG_HOME:-"$HOME/.config"}

canonical_home=$(realpath -m -- "$HOME")
canonical_install=$(realpath -m -- "$install_dir")
case "$canonical_install" in
	"$canonical_home"/*) [ "$canonical_home" != / ] && [ "$canonical_install" != "$canonical_home" ] ;;
    *) printf 'Install directory must be inside HOME: %s\n' "$install_dir" >&2; exit 2 ;;
esac
case "$canonical_install" in
	*'"'*|*'
'*) printf 'Install directory contains unsupported launcher characters\n' >&2; exit 2 ;;
esac

reject_symlink_components() {
    path=$1
    case "$path" in /*) current=/; rest=${path#/} ;; *) current=$PWD; rest=$path ;; esac
    while [ -n "$rest" ]; do
        case "$rest" in */*) part=${rest%%/*}; rest=${rest#*/} ;; *) part=$rest; rest= ;; esac
        [ -n "$part" ] || continue
        [ "$part" != . ] || continue
        if [ "$part" = .. ]; then current=$(dirname -- "$current"); continue; fi
        [ "$current" = / ] && current="/$part" || current="$current/$part"
        [ ! -L "$current" ] || { printf 'Managed path contains a symlink: %s\n' "$current" >&2; return 1; }
    done
}
reject_nested_mounts() {
    root=$(realpath -m -- "$1")
    if command -v findmnt >/dev/null 2>&1; then
        mounts=$(findmnt -R -n -o TARGET -- "$root" 2>/dev/null || true)
    elif [ -r /proc/self/mountinfo ]; then
        mounts=$(awk '{print $5}' /proc/self/mountinfo)
    else
        printf 'Cannot inspect mount table for managed root: %s\n' "$root" >&2
        return 1
    fi
    while IFS= read -r mount; do
        [ -n "$mount" ] || continue
        mount=$(printf '%s' "$mount" | sed 's/\\\\040/ /g; s/\\\\011/\t/g; s/\\\\134/\\\\/g')
        [ "$mount" = "$root" ] && continue
        case "$mount" in
            "$root"/*) printf 'Refusing to operate on managed root containing nested mount: %s\n' "$mount" >&2; return 1 ;;
        esac
    done <<EOF
$mounts
EOF
}
case "$install_dir" in /*) install_absolute=$install_dir ;; *) install_absolute="$PWD/$install_dir" ;; esac
reject_symlink_components "$install_absolute"
reject_symlink_components "$data_home"
reject_symlink_components "$config_home"
install_dir=$canonical_install
if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    reject_nested_mounts "$install_dir" || exit 2
fi
executable="$install_dir/replicaro"
parent_dir=$(dirname -- "$install_dir")

strict_managed_layout() {
    reject_nested_mounts "$install_dir" || return 1
    [ -d "$install_dir" ] && [ ! -L "$install_dir" ] || return 1
    [ -f "$install_dir/$ownership_marker" ] && [ "$(cat "$install_dir/$ownership_marker")" = "$ownership_marker_value" ] || return 1
    [ -f "$install_dir/replicaro" ] && [ ! -L "$install_dir/replicaro" ] && [ -x "$install_dir/replicaro" ] || return 1
    if [ -f "$install_dir/LICENSES.txt" ] && [ ! -L "$install_dir/LICENSES.txt" ] &&
       [ "$(find "$install_dir" -mindepth 1 -maxdepth 1 -print | wc -l)" -eq 3 ]; then
        entries="$ownership_marker replicaro LICENSES.txt"
    elif [ -d "$install_dir/licenses" ] && [ ! -L "$install_dir/licenses" ] &&
         [ -d "$install_dir/provenance" ] && [ ! -L "$install_dir/provenance" ] &&
         [ "$(find "$install_dir" -mindepth 1 -maxdepth 1 -print | wc -l)" -eq 4 ]; then
        entries="$ownership_marker replicaro licenses provenance"
    else
        return 1
    fi
    for entry in $entries; do
        find "$install_dir/$entry" \( -type l -o \( ! -type f ! -type d \) \) -print -quit | grep -q . && return 1
    done
    return 0
}

if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    reject_nested_mounts "$install_dir"
    [ -d "$install_dir" ] && [ ! -L "$install_dir" ] || { printf 'Install directory is not a managed directory: %s\n' "$install_dir" >&2; exit 2; }
    if strict_managed_layout; then
        fresh_install=0
    elif [ "$(find "$install_dir" -mindepth 1 -maxdepth 1 -print | wc -l)" -eq 0 ]; then
        fresh_install=1
    else
        printf 'Refusing to replace unmanaged nonempty install directory: %s\n' "$install_dir" >&2
        exit 2
    fi
else
    fresh_install=1
fi
mkdir -p -- "$parent_dir" "$data_home/applications" "$data_home/icons/hicolor/256x256/apps" "$config_home/systemd/user"

escaped_executable=$(printf '%s\n' "$executable" | sed 's/[&|\\]/\\&/g; s/%/%%/g')
desktop_target="$data_home/applications/replicaro.desktop"
desktop_content=$(sed "s|@REPLICARO_EXECUTABLE@|$escaped_executable|g" "$bundle_dir/share/applications/replicaro.desktop")
unit_target="$config_home/systemd/user/replicaro.service"
unit_content=$(sed "s|@REPLICARO_EXECUTABLE@|$escaped_executable|g" "$bundle_dir/share/systemd/user/replicaro.service")
legacy_unit_content=$(printf '%s\n' "$unit_content" | sed '1{/^# Managed by Replicaro$/d;}')

validate_startup_target() {
	target=$1
	legacy=$2
	label=$3
	[ ! -L "$target" ] || { printf 'Refusing to replace symlinked %s\n' "$label" >&2; return 1; }
	[ ! -e "$target" ] && return 0
	[ -f "$target" ] || { printf 'Refusing to replace non-file %s\n' "$label" >&2; return 1; }
	[ "$(sed -n '1p' "$target")" = '# Managed by Replicaro' ] && return 0
	[ "$(cat "$target")" = "$legacy" ] && return 0
	printf 'Refusing to replace unmanaged %s\n' "$label" >&2
	return 1
}
validate_startup_target "$desktop_target" "$desktop_content" 'desktop entry'
validate_startup_target "$unit_target" "$legacy_unit_content" 'systemd unit'

stage_dir=$(mktemp -d "$parent_dir/.replicaro-install.XXXXXX")
cleanup_stage() { rm -rf -- "$stage_dir"; }
trap cleanup_stage EXIT HUP INT TERM

cp "$bundle_dir/replicaro" "$stage_dir/replicaro"
cp "$bundle_dir/LICENSES.txt" "$stage_dir/LICENSES.txt"
chmod 700 "$stage_dir/replicaro"
printf '%s\n' "$ownership_marker_value" > "$stage_dir/$ownership_marker"
chmod 600 "$stage_dir/$ownership_marker"
icon_target="$data_home/icons/hicolor/256x256/apps/replicaro.png"
[ ! -L "$icon_target" ] || { printf 'Refusing to replace symlinked icon path\n' >&2; exit 2; }
cp "$bundle_dir/share/icons/hicolor/256x256/apps/replicaro.png" "$icon_target"

if [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    [ -d "$install_dir" ] && [ ! -L "$install_dir" ] || { printf 'Install directory is not a managed directory: %s\n' "$install_dir" >&2; exit 2; }
    backup_dir=$(mktemp -d "$parent_dir/.replicaro-backup.XXXXXX")
    rmdir -- "$backup_dir"
    mv -- "$install_dir" "$backup_dir"
    if ! mv -- "$stage_dir" "$install_dir"; then
        mv -- "$backup_dir" "$install_dir"
        exit 1
    fi
    reject_nested_mounts "$backup_dir" || { mv -- "$install_dir" "$stage_dir"; mv -- "$backup_dir" "$install_dir"; exit 2; }
    rm -rf -- "$backup_dir"
else
    mv -- "$stage_dir" "$install_dir"
fi
trap - EXIT HUP INT TERM
{ printf '%s\n' '# Managed by Replicaro'; printf '%s\n' "$desktop_content"; } > "$desktop_target"
{ printf '%s\n' '# Managed by Replicaro'; printf '%s\n' "$legacy_unit_content"; } > "$unit_target"

if [ "${REPLICARO_SKIP_SYSTEMD:-0}" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload || true
fi
printf 'Replicaro installed at %s\n' "$install_dir"
if [ "$fresh_install" -eq 1 ] && [ "$(id -u)" -ne 0 ] &&
    [ -n "${DBUS_SESSION_BUS_ADDRESS:-}" ] &&
    { [ -n "${DISPLAY:-}" ] || [ -n "${WAYLAND_DISPLAY:-}" ]; }; then
    launch_requested=0
    if command -v nohup >/dev/null 2>&1; then
        if nohup "$executable" </dev/null >/dev/null 2>&1 & then
            launch_requested=1
        fi
    else
        if "$executable" </dev/null >/dev/null 2>&1 & then
            launch_requested=1
        fi
    fi
    if [ "$launch_requested" -eq 1 ]; then
        printf 'Replicaro launch requested in the current desktop session\n'
    else
        printf 'Replicaro was installed but automatic launch could not be requested\n' >&2
    fi
fi
