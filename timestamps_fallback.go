//go:build !linux && !darwin && !freebsd

package main

import (
	"reflect"
	"syscall"
)

func statTimes(sb *syscall.Stat_t) (syscall.Timespec, syscall.Timespec, bool) {
	v := reflect.ValueOf(sb).Elem()

	atim := v.FieldByName("Atim")
	mtim := v.FieldByName("Mtim")
	if atim.IsValid() && mtim.IsValid() {
		return atim.Interface().(syscall.Timespec), mtim.Interface().(syscall.Timespec), true
	}

	atim = v.FieldByName("Atimespec")
	mtim = v.FieldByName("Mtimespec")
	if atim.IsValid() && mtim.IsValid() {
		return atim.Interface().(syscall.Timespec), mtim.Interface().(syscall.Timespec), true
	}

	return syscall.Timespec{}, syscall.Timespec{}, false
}

func restoreFileTimes(fd int, atime, mtime syscall.Timespec) error {
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(syscall.TimespecToNsec(atime)),
		syscall.NsecToTimeval(syscall.TimespecToNsec(mtime)),
	}
	return futimesFile(fd, tv)
}
