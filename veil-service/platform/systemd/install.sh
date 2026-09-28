#!/bin/sh
# Run from the extracted release. --destdir stages files without host actions.
set -eu

usage() { echo 'usage: install.sh install|uninstall [--destdir ABSOLUTE_DIRECTORY]' >&2; exit 2; }
[ "$#" -ge 1 ] || usage
action=$1
shift
case "$action" in install|uninstall) ;; *) usage ;; esac
dest=
if [ "$#" -ne 0 ]; then
    [ "$#" -eq 2 ] && [ "$1" = --destdir ] || usage
    case "$2" in /*) ;; *) usage ;; esac
    [ -d "$2" ] || { echo 'destdir must already exist' >&2; exit 1; }
    dest=$(CDPATH='' cd -- "$2" && pwd -P)
    [ "$dest" != / ] || { echo 'use no --destdir for a live installation' >&2; exit 1; }
fi
source=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
files='bin/veild bin/veilctl bin/veil-system lib/systemd/system/veil@.service lib/sysusers.d/veil.conf share/veil/manifest.json share/veil/LICENSE share/veil/THIRD_PARTY.md share/veil/README.md share/veil/uninstall.sh'
if [ "$action" = install ]; then
    for file in $files; do
        input="$source/$file"
        case "$file" in share/veil/uninstall.sh) input="$source/install.sh" ;; esac
        [ -f "$input" ] && [ -r "$input" ] || { echo "missing package file: $input" >&2; exit 1; }
    done
fi
if [ -z "$dest" ]; then
    [ "$(id -u)" -eq 0 ] || { echo 'live installation requires root' >&2; exit 1; }
    command -v systemctl >/dev/null
    command -v systemd-sysusers >/dev/null
    if [ "$action" = uninstall ]; then
        running=$(systemctl list-units --all --plain --no-legend --state=active,activating,deactivating,reloading 'veil@*.service')
        # list-unit-files reports templates, not enabled instances (249+).
        enabled=
        for link in /etc/systemd/system/*.wants/veil@*.service /etc/systemd/system/*.requires/veil@*.service /run/systemd/system/*.wants/veil@*.service /run/systemd/system/*.requires/veil@*.service; do
            if [ -L "$link" ]; then enabled=$link; fi
        done
        if [ -n "$running$enabled" ]; then
            echo 'disable Veil instances with veil-system INSTANCE disable before uninstalling' >&2
            exit 1
        fi
    fi
fi

# Refuse redirected paths; never follow a pre-existing target symlink as root.
for dir in /usr /usr/local /usr/local/bin /usr/local/lib /usr/local/lib/systemd /usr/local/lib/systemd/system /usr/local/lib/sysusers.d /usr/local/share /usr/local/share/veil; do
    [ ! -L "$dest$dir" ] || { echo "refusing symlink: $dest$dir" >&2; exit 1; }
    if [ "$action" = install ] && [ ! -d "$dest$dir" ]; then install -d -m 0755 "$dest$dir"; fi
done
for file in $files; do
    target="$dest/usr/local/$file"
    [ ! -L "$target" ] || { echo "refusing symlink: $target" >&2; exit 1; }
    [ ! -e "$target" ] || [ -f "$target" ] || { echo "not a file: $target" >&2; exit 1; }
done

temporary=
trap '[ -z "$temporary" ] || rm -f -- "$temporary"' EXIT
trap 'exit 1' HUP INT TERM
for file in $files; do
    target="$dest/usr/local/$file"
    if [ "$action" = uninstall ]; then
        rm -f -- "$target"
        continue
    fi
    input="$source/$file"
    case "$file" in
        share/veil/uninstall.sh) input="$source/install.sh" ;;
    esac
    mode=0644
    case "$file" in bin/*|*/uninstall.sh) mode=0755 ;; esac
    temporary=$(mktemp "$target.XXXXXXXX")
    install -m "$mode" "$input" "$temporary"
    mv -f -- "$temporary" "$target"
    temporary=
done
if [ -z "$dest" ]; then
    if [ "$action" = install ]; then systemd-sysusers /usr/local/lib/sysusers.d/veil.conf; fi
    systemctl daemon-reload
fi
echo "$action complete; instance configuration is preserved"
