package apps

import (
	"errors"
	"fmt"
	"net"
)

// portRangeStart/End bound the host-port window the platform allocates in-VM app
// published ports from. It sits well above the ephemeral range's usual start and
// the platform's own service ports (14000/18787/…), so an allocation is unlikely
// to clash with an unrelated listener and never with a service-tier port.
const (
	portRangeStart = 21000
	portRangeEnd   = 21999
)

// ErrNoFreePort is returned when no free, unreserved host port could be found in
// the allocation window (→ exit 4).
var ErrNoFreePort = errors.New("no free host port available for the app")

// portChecker reports whether a TCP port on the host is currently bindable. It is
// injectable so tests can drive allocation deterministically without touching the
// real network; the real implementation tries to listen on the port.
type portChecker func(port int) bool

// realPortFree reports whether a host TCP port is currently free by attempting to
// bind it on the loopback interface and immediately releasing it. A bind error
// means the port is in use (or not permitted), so it is treated as not free.
func realPortFree(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// allocatePort returns a host port that is (a) not in reserved and (b) reported
// free by isFree, scanning the allocation window in order so allocation is
// deterministic for a given reserved set. reserved holds the ports already
// allocated to other apps (in this and — when the caller passes them — other
// workspaces) so two running microVMs never collide on a published host port.
func allocatePort(reserved map[int]bool, isFree portChecker) (int, error) {
	for port := portRangeStart; port <= portRangeEnd; port++ {
		if reserved[port] {
			continue
		}
		if isFree(port) {
			return port, nil
		}
	}
	return 0, ErrNoFreePort
}
