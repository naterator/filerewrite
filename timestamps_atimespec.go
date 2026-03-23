//go:build darwin || freebsd || netbsd

package main

import "syscall"

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	return sb.Atimespec, sb.Mtimespec, true
}

func restoreFileTimes(path string, atime, mtime syscall.Timespec) error {
	return syscall.UtimesNano(path, []syscall.Timespec{atime, mtime})
}
