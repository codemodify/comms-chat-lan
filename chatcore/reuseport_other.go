//go:build !unix

package chatcore

import "syscall"

// reusePort is a no-op where SO_REUSEPORT is not available. One daemon per
// machine still works; a second one will fail to bind the beacon port.
func reusePort(_, _ string, _ syscall.RawConn) error { return nil }
