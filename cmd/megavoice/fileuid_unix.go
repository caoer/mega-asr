//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileUID is a file's owner, where the platform reports one.
func fileUID(st fs.FileInfo) (int, bool) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(s.Uid), true
}
