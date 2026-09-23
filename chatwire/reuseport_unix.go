//go:build unix

package chatwire

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePort sets SO_REUSEADDR and SO_REUSEPORT on the beacon socket before
// it is bound, so several copies of the app on one machine can all join the
// group port. Trying the app out means running two of them side by side,
// and without this the second one fails at startup.
func reusePort(_, _ string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
			serr = err
			return
		}
		if err := unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err != nil {
			serr = err
		}
	})
	if err != nil {
		return err
	}
	return serr
}
