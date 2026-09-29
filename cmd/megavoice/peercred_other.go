//go:build !linux && !darwin

package main

import (
	"fmt"
	"net"
	"runtime"
)

// peerCred has no kernel report of the caller off Linux, so the control
// socket refuses every caller there.
func peerCred(net.Conn) (peer, error) {
	return peer{}, fmt.Errorf("peer credentials: not implemented on %s", runtime.GOOS)
}
