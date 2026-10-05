//go:build !darwin && !linux

package policystore_test

import "net"

func dialSmallReceiveWindow(address string) (net.Conn, error) {
	c, err := net.Dial("tcp", address)
	if err != nil {
		return nil, err
	}
	if err = c.(*net.TCPConn).SetReadBuffer(1024); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}
