//go:build darwin || linux

package policystore_test

import (
	"net"
	"syscall"
)

// Set the receive buffer before the handshake, so TCP window scaling cannot
// advertise the OS default window before this no-reading client takes over.
// The evidence still comes from the server's native TCP Write timeout.
func dialSmallReceiveWindow(address string) (net.Conn, error) {
	d := net.Dialer{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		if err := raw.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1024)
		}); err != nil {
			return err
		}
		return optionErr
	}}
	return d.Dial("tcp", address)
}
