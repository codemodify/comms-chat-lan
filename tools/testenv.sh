#!/bin/bash
# testenv.sh CMD... — runs CMD where it cannot reach the desktop it runs on:
# no Wayland or X11 display, a private D-Bus session bus that can start no
# services, and a runtime dir of its own, so nothing falls back to the real
# $XDG_RUNTIME_DIR/bus (godbus and platform.sessionBusAddress both do when
# DBUS_SESSION_BUS_ADDRESS is unset).
#
#   tools/testenv.sh go test ./...
#
# The bus is torn down when CMD exits; CMD's exit status is kept.
set -euo pipefail
tmp=$(mktemp -d)
bus_pid=
cleanup() { [ -n "$bus_pid" ] && kill "$bus_pid" 2>/dev/null; rm -rf "$tmp"; }
trap cleanup EXIT

# A session bus with no <servicedir>: a test that asks for a service gets
# "not found" at once instead of something being auto-started.
cat > "$tmp/bus.conf" <<EOF
<!DOCTYPE busconfig PUBLIC "-//freedesktop//DTD D-Bus Bus Configuration 1.0//EN"
 "http://www.freedesktop.org/standards/dbus/1.0/busconfig.dtd">
<busconfig>
  <type>session</type>
  <listen>unix:tmpdir=$tmp</listen>
  <policy context="default">
    <allow send_destination="*" eavesdrop="true"/>
    <allow eavesdrop="true"/>
    <allow own="*"/>
  </policy>
</busconfig>
EOF
mkdir -m 700 "$tmp/run"
dbus-daemon --config-file="$tmp/bus.conf" --nofork --print-address=3 3>"$tmp/addr" &
bus_pid=$!
for _ in $(seq 100); do
  [ -s "$tmp/addr" ] && break
  sleep 0.05
done
addr=$(head -1 "$tmp/addr")
[ -n "$addr" ] || { echo "testenv: the private bus did not start" >&2; exit 1; }

env -u WAYLAND_DISPLAY -u DISPLAY -u WAYLAND_SOCKET \
  DBUS_SESSION_BUS_ADDRESS="$addr" XDG_RUNTIME_DIR="$tmp/run" \
  XDG_CONFIG_HOME="$tmp/config" XDG_CACHE_HOME="$tmp/cache" XDG_DATA_HOME="$tmp/data" \
  "$@"
