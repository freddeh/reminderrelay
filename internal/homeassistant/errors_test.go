package homeassistant

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"syscall"
	"testing"
)

func TestIsNoRouteToHost(t *testing.T) {
	// The shape net/http actually produces: url.Error → net.OpError → os.SyscallError → Errno.
	real := &url.Error{
		Op:  "Get",
		URL: "http://192.168.0.112:8123/api/",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)},
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"bare errno", syscall.EHOSTUNREACH, true},
		{"real http client error chain", real, true},
		{"wrapped by Retry-style fmt.Errorf %w", fmt.Errorf("all 3 attempts failed: %w", real), true},
		{"flattened to text (errno lost)", errors.New("dial tcp 192.168.0.112:8123: connect: no route to host"), true},
		{"other network error", syscall.ECONNREFUSED, false},
		{"unrelated error", errors.New("401 Unauthorized"), false},
	}
	for _, tt := range tests {
		if got := IsNoRouteToHost(tt.err); got != tt.want {
			t.Errorf("%s: IsNoRouteToHost = %v, want %v", tt.name, got, tt.want)
		}
	}
}
