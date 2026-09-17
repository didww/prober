#!/bin/sh
set -e
if [ -d /run/systemd/system ] && [ "$1" = "remove" ]; then
    systemctl stop @PKG@ || true
fi
exit 0
