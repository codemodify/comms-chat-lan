//go:build linux

package chatcore

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

// Socket hardening for comms-chatd.
//
// The daemon exposes the whole message history, the roster and the send
// path over this socket with no authentication of its own, so the socket
// itself is the authorisation boundary:
//
//   - the parent directory is created 0700 and checked on every start
//   - the socket file is bound under a tight umask and chmodded 0600
//   - a lock file stops a second daemon stealing the path from a running
//     one, instead of the older habit of removing the socket and hoping
//   - every accepted connection must come from our own uid (SO_PEERCRED)
//
// None of it protects against anything on the network; see docs/security.md.

// socketLock is an advisory lock held for the lifetime of a listener.
type socketLock struct {
	f *os.File
}

func (l *socketLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// lockSocket takes the per-socket lock file. An error means another
// comms-chatd is already serving that path.
func lockSocket(socket string) (*socketLock, error) {
	path := socket + ".lock"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("chat: another comms-chatd is already serving %s (%w)", socket, err)
	}
	if err := f.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	}
	return &socketLock{f: f}, nil
}

// prepareSocketDir makes sure the directory holding the socket is ours and
// owner-only. A world-writable directory (/tmp, when XDG_RUNTIME_DIR is
// unset) would let another user pre-create the path.
func prepareSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("chat: socket directory %s is owned by uid %d, not %d", dir, sys.Uid, os.Getuid())
	}
	if ok && st.Mode().Perm()&0o077 != 0 && isPrivateSocketDir(dir) {
		_ = os.Chmod(dir, 0o700)
	}
	return nil
}

// isPrivateSocketDir is true for a directory this package owns exclusively.
// Shared roots (/tmp, $XDG_RUNTIME_DIR itself) are never re-chmodded:
// tightening them would break every other program using them.
func isPrivateSocketDir(dir string) bool {
	clean := filepath.Clean(dir)
	for _, shared := range []string{filepath.Clean(os.TempDir()), "/", "/run", "/var/run"} {
		if clean == shared {
			return false
		}
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && clean == filepath.Clean(rt) {
		return false
	}
	return true
}

func listenSocket(socket string) (net.Listener, *socketLock, error) {
	if err := prepareSocketDir(socket); err != nil {
		return nil, nil, err
	}
	lock, err := lockSocket(socket)
	if err != nil {
		return nil, nil, err
	}
	// The lock is ours, so any socket file left behind is stale.
	if err := os.RemoveAll(socket); err != nil {
		lock.release()
		return nil, nil, err
	}
	// Bind under a restrictive umask, so there is no window in which the
	// socket is connectable by another user.
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		lock.release()
		return nil, nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = ln.Close()
		lock.release()
		return nil, nil, err
	}
	return ln, lock, nil
}

// peerAllowed reports whether conn's peer is our own uid. Root is allowed
// too: it can read the history files directly anyway.
func peerAllowed(conn net.Conn) (bool, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		// Not a Unix socket (a test may use a pipe): nothing to check.
		return true, nil
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false, err
	}
	var cred *syscall.Ucred
	var credErr error
	err = raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return false, err
	}
	if credErr != nil {
		return false, credErr
	}
	if cred == nil {
		return false, fmt.Errorf("chat: no peer credentials on the socket")
	}
	return int(cred.Uid) == os.Getuid() || cred.Uid == 0, nil
}
