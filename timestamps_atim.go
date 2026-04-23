//go:build linux || openbsd

package main

import (
	"fmt"
	"syscall"
)

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	return sb.Atim, sb.Mtim, true
}

func restoreFileTimes(fd int, atime, mtime syscall.Timespec) error {
	return syscall.UtimesNano(fmt.Sprintf("/dev/fd/%d", fd), []syscall.Timespec{atime, mtime})
}
