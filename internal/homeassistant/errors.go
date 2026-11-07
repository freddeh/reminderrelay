package homeassistant

import (
	"errors"
	"strings"
	"syscall"
)

// IsNoRouteToHost reports whether err is the OS-level "no route to host"
// (EHOSTUNREACH). For a LAN address that is reachable from a terminal this is
// usually not a network problem at all: macOS 15+ blocks processes that lack
// "Local Network" permission with exactly this error — which launchd-started
// bare binaries can never obtain. The string fallback covers error chains
// that flatten the original errno into text.
func IsNoRouteToHost(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EHOSTUNREACH) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no route to host")
}
