package main

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// The kernel reports the caller: this process's uid, gid and every
// supplementary group, read from the socket, not from anything sent on it.
func TestPeerCred(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err := peerCred(s)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != os.Getuid() || p.GID != os.Getgid() {
		t.Errorf("peer %+v, want uid %d gid %d", p, os.Getuid(), os.Getgid())
	}
	gs, _ := os.Getgroups()
	for _, g := range gs {
		if !slices.Contains(p.Groups, g) {
			t.Errorf("groups %v lack %d (process groups %v)", p.Groups, g, gs)
		}
	}
}
