//go:build linux

package main

import "syscall"

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	return sb.Atim, sb.Mtim, true
}

func restoreFileTimes(fd int, atime, mtime syscall.Timespec) error {
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(syscall.TimespecToNsec(atime)),
		syscall.NsecToTimeval(syscall.TimespecToNsec(mtime)),
	}
	return futimesFile(fd, tv)
}
