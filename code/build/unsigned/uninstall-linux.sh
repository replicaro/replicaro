#!/bin/sh
set -eu

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
    *) printf 'Refusing to remove unexpected install directory: %s\n' "$install_dir" >&2; exit 2 ;;
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

managed_install=0
if [ -d "$install_dir" ] && [ ! -L "$install_dir" ]; then
    if strict_managed_layout; then
        managed_install=1
    else
        printf 'Refusing to remove unmanaged install directory: %s\n' "$install_dir" >&2
        exit 2
    fi
elif [ -e "$install_dir" ] || [ -L "$install_dir" ]; then
    printf 'Refusing to remove non-directory install path: %s\n' "$install_dir" >&2
    exit 2
fi

executable="$install_dir/replicaro"
desktop_target="$data_home/applications/replicaro.desktop"
unit_target="$config_home/systemd/user/replicaro.service"
legacy_desktop="[Desktop Entry]
Type=Application
Name=Replicaro
Exec=\"$executable\"
Terminal=false
Categories=Utility;Archiving;
Icon=replicaro"
legacy_unit="[Unit]
Description=Replicaro backup companion
After=network-online.target

[Service]
Type=simple
ExecStart=\"$executable\"
Restart=on-failure

[Install]
WantedBy=default.target"
owned_startup_file() {
	target=$1
	legacy=$2
	[ -f "$target" ] && [ ! -L "$target" ] || return 1
	[ "$(sed -n '1p' "$target")" = '# Managed by Replicaro' ] || [ "$(cat "$target")" = "$legacy" ]
}
unit_owned=0
owned_startup_file "$unit_target" "$legacy_unit" && unit_owned=1
if [ "$unit_owned" = 1 ] && [ "${REPLICARO_SKIP_SYSTEMD:-0}" != 1 ] && command -v systemctl >/dev/null 2>&1; then
	systemctl --user disable --now replicaro.service || true
fi
if owned_startup_file "$desktop_target" "$legacy_desktop"; then rm -f -- "$desktop_target"; fi
if [ "$unit_owned" = 1 ]; then rm -f -- "$unit_target"; fi
if [ "$managed_install" = 1 ]; then
	reject_nested_mounts "$install_dir" || exit 2
	[ ! -L "$data_home/icons/hicolor/256x256/apps/replicaro.png" ] && rm -f -- "$data_home/icons/hicolor/256x256/apps/replicaro.png"
	rm -rf -- "$install_dir"
fi
if [ "${REPLICARO_SKIP_SYSTEMD:-0}" != 1 ] && command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload || true
fi
printf 'Replicaro removed from %s\n' "$install_dir"
