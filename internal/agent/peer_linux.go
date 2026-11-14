//go:build linux

package agent

import (
	"net"
	"syscall"
)

func peerUID(conn net.Conn) (int, error) {
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, syscall.EINVAL
	}
	var uid int
	var controlErr error
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return -1, err
	}
	err = raw.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			controlErr = err
			return
		}
		uid = int(cred.Uid)
	})
	if err != nil {
		return -1, err
	}
	if controlErr != nil {
		return -1, controlErr
	}
	return uid, nil
}
