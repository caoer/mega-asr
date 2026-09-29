package main

import (
	"errors"
	"fmt"
	"net"
	"unsafe"

	"golang.org/x/sys/unix"
)

// peerCred is the caller on a unix socket as the kernel reports it:
// SO_PEERCRED's uid and gid, and SO_PEERGROUPS' supplementary groups (none
// on a kernel before 4.13, which lacks it).
func peerCred(c net.Conn) (peer, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return peer{}, fmt.Errorf("peer credentials: %T is not a unix socket", c)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return peer{}, err
	}
	var p peer
	var cerr error
	err = raw.Control(func(fd uintptr) {
		u, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if err != nil {
			cerr = fmt.Errorf("SO_PEERCRED: %w", err)
			return
		}
		p.UID, p.GID = int(u.Uid), int(u.Gid)
		p.Groups, cerr = peerGroups(int(fd))
	})
	if err == nil {
		err = cerr
	}
	return p, err
}

func peerGroups(fd int) ([]int, error) {
	buf := make([]uint32, 32)
	for {
		n := uint32(4 * len(buf))
		_, _, e := unix.Syscall6(unix.SYS_GETSOCKOPT, uintptr(fd), unix.SOL_SOCKET, unix.SO_PEERGROUPS,
			uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n)), 0)
		switch {
		case e == unix.ERANGE && int(n/4) > len(buf):
			buf = make([]uint32, n/4)
			continue
		case errors.Is(e, unix.ENOPROTOOPT):
			return nil, nil
		case e != 0:
			return nil, fmt.Errorf("SO_PEERGROUPS: %w", e)
		}
		gs := make([]int, n/4)
		for i := range gs {
			gs[i] = int(buf[i])
		}
		return gs, nil
	}
}
