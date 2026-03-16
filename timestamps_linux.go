//go:build linux

package main

import "syscall"

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	return sb.Atim, sb.Mtim, true
}

func restoreFileTimes(path string, atime, mtime syscall.Timespec) error {
	return syscall.UtimesNano(path, []syscall.Timespec{atime, mtime})
}
