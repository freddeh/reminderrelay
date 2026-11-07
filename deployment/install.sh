#!/usr/bin/env bash
# install.sh — build and (re)install ReminderRelay as a launchd user agent.
#
# Usage: bash deployment/install.sh
#
# Thin wrapper: the actual installation (CLI binary in /usr/local/bin, the
# signed ReminderRelay.app bundle the daemon runs from, the launchd plist, and
# loading the job) lives in `reminderrelay install-daemon`, so the setup
# wizard, `just install` and this script can never drift apart.
#
# Requirements: devbox (or Go 1.24+ in PATH), macOS, iCloud sign-in.
set -euo pipefail

cd "$(dirname "$0")/.."

echo "→ Building reminderrelay…"
if command -v devbox &>/dev/null; then
    devbox run -- env CGO_ENABLED=1 go build -buildvcs=false -o reminderrelay ./cmd/reminderrelay
else
    CGO_ENABLED=1 go build -buildvcs=false -o reminderrelay ./cmd/reminderrelay
fi

./reminderrelay install-daemon

echo ""
echo "If you haven't created a config file yet, run: ./reminderrelay setup"
