# ReminderRelay justfile
# Run `just --list` to see all available recipes.

set shell := ["zsh", "-cu"]

binary      := "reminderrelay"
state_db    := env('HOME') / ".local/share/reminderrelay/state.db"

# List available recipes
default:
    @just --list

# ── Build ──────────────────────────────────────────────────────────────────────

# Build the binary
build:
    CGO_ENABLED=1 go build -buildvcs=false -o {{binary}} ./cmd/reminderrelay

# Build with race detector (slower, for CI)
build-race:
    CGO_ENABLED=1 go build -buildvcs=false -race -o {{binary}} ./cmd/reminderrelay

# ── Run ────────────────────────────────────────────────────────────────────────

# Start the daemon (blocking)
run: build
    ./{{binary}} daemon

# Run a single sync cycle and exit
sync-once: build
    ./{{binary}} sync-once

# ── Test & Lint ────────────────────────────────────────────────────────────────

# Run unit tests with race detector
test:
    CGO_ENABLED=1 go test -buildvcs=false -race ./...

# Run integration tests (requires local Reminders + HA instance)
test-integration:
    CGO_ENABLED=1 go test -buildvcs=false -race -tags integration ./...

# Run linter
lint:
    golangci-lint run ./...

# Run linter and auto-fix where possible
lint-fix:
    golangci-lint run --fix ./...

# Format all Go source files
fmt:
    gofmt -w .

# ── Install / Uninstall ────────────────────────────────────────────────────────

# The daemon runs from a signed ReminderRelay.app bundle, not the bare binary:
# macOS only grants Local Network / Reminders access to something it can
# identify as an app. `install-daemon` sets up CLI binary + bundle + plist.

# Build, then (re)install the daemon: CLI binary, signed app bundle, launchd plist
install: build
    ./{{binary}} install-daemon

# Stop the daemon and remove plist, app bundle and binary (config + state DB stay)
uninstall: build
    ./{{binary}} uninstall

# ── Database ───────────────────────────────────────────────────────────────────

# Open an interactive SQLite shell against the state database
db:
    sqlite3 {{state_db}}

# Dump the sync_items table in a readable format
db-dump:
    sqlite3 -column -header {{state_db}} "SELECT * FROM sync_items ORDER BY last_synced_at DESC;"

# ── Maintenance ────────────────────────────────────────────────────────────────

# Remove build artifacts
clean:
    -rm -f {{binary}}

# Download and tidy Go module dependencies
tidy:
    go mod tidy

# Print tool versions (useful for debugging CI)
versions:
    @go version
    @just --version
    @golangci-lint --version
    @sqlite3 --version
