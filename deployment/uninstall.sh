#!/usr/bin/env bash
# uninstall.sh — stop and remove ReminderRelay.
#
# Usage: bash deployment/uninstall.sh [--purge]
#
# --purge also removes the config file, state database and logs.
#
# Thin wrapper around `reminderrelay uninstall`, which unloads the launchd job,
# stops the running app, and removes the plist, app bundle and binary.
set -euo pipefail

if command -v reminderrelay &>/dev/null; then
    exec reminderrelay uninstall "$@"
fi

cd "$(dirname "$0")/.."
if [[ ! -x ./reminderrelay ]]; then
    echo "reminderrelay binary not found in PATH or repo root — run 'just build' first." >&2
    exit 1
fi
exec ./reminderrelay uninstall "$@"
