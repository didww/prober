#!/bin/sh
# @PKG@ is replaced with the package name (prober-backend / prober-agent) when
# the .deb is assembled.
set -e
PREFIX="/opt/@PKG@"

# The package ships config.example.yml only; the real config.yml (with the
# token / cookie secret) is written by whoever deploys and never clobbered.
if [ -f "$PREFIX/etc/config.yml" ]; then
    chown root:root "$PREFIX/etc/config.yml"
    chmod 0600 "$PREFIX/etc/config.yml"
fi
chmod 0750 "$PREFIX/etc" 2>/dev/null || true

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload
    if systemctl is-active --quiet @PKG@; then
        systemctl restart @PKG@
    fi
fi
exit 0
