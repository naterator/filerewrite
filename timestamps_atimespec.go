//go:build darwin || freebsd || netbsd

package main

import (
	"fmt"
	"syscall"
)

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	return sb.Atimespec, sb.Mtimespec, true
}

func restoreFileTimes(fd int, atime, mtime syscall.Timespec) error {
	return syscall.UtimesNano(fmt.Sprintf("/dev/fd/%d", fd), []syscall.Timespec{atime, mtime})
}
