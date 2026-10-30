#!/usr/bin/env bash
# Install and enable logr-server as a systemd user service so it starts at login.
#
# Usage:
#   scripts/install-user-service.sh          # install + enable + start
#   scripts/install-user-service.sh --uninstall
#
# Requires: systemd user instance (standard on most Linux desktops).

set -euo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${REPO_DIR}/bin/logr-server"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_PATH="${UNIT_DIR}/logr-server.service"

if [[ "${1:-}" == "--uninstall" ]]; then
    systemctl --user disable --now logr-server.service 2>/dev/null || true
    rm -f "${UNIT_PATH}"
    systemctl --user daemon-reload
    echo "Removed ${UNIT_PATH}"
    exit 0
fi

if [[ ! -x "${BINARY}" ]]; then
    echo "logr-server binary not found at ${BINARY}" >&2
    echo "Run 'make' in ${REPO_DIR} first." >&2
    exit 1
fi

mkdir -p "${UNIT_DIR}"

cat > "${UNIT_PATH}" <<EOF
[Unit]
Description=logr-server (log ingest + query daemon)
After=default.target

[Service]
Type=simple
ExecStart=${BINARY}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now logr-server.service
systemctl --user --no-pager status logr-server.service || true

cat <<EOF

Installed: ${UNIT_PATH}
Logs:      journalctl --user -u logr-server -f
Optional:  loginctl enable-linger "\$USER"   # keep running when logged out
EOF
