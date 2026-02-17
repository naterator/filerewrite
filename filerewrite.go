package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"reflect"
	"syscall"
)

var verbose bool
var bufferSizeMB int

func logWarning(format string, args ...any) {
	log.Printf(format, args...)
}

func logWarningWithError(err error, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("%s: %v.", msg, err)
}

func logVerbose(format string, args ...any) {
	if !verbose {
		return
	}
	log.Printf(format, args...)
}

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

func rewriteFile(path string, bufferSizeBytes int) bool {
	buf := make([]byte, bufferSizeBytes)
	ret := false

	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		logWarningWithError(err, "Unable to open %s", path)
		return false
	}
	defer syscall.Close(fd)

	var sb syscall.Stat_t
	if err := syscall.Fstat(fd, &sb); err != nil {
		logWarningWithError(err, "Unable to stat %s", path)
		return false
	}
	if (sb.Mode & syscall.S_IFMT) != syscall.S_IFREG {
		logWarning("%s is not a regular file, skipping.", path)
		return false
	}

	var offset int64
	for {
		rdone, err := syscall.Pread(fd, buf, offset)
		if err != nil {
			logWarningWithError(err, "Read from %s at offset %d failed", path, offset)
			return false
		}
		if rdone == 0 {
			break
		}
		logVerbose("Read %d from %s at offset %d.", rdone, path, offset)

		wdone, err := syscall.Pwrite(fd, buf[:rdone], offset)
		if err != nil {
			logWarningWithError(err, "Write %s at offset %d failed", path, offset)
			return false
		}
		if wdone == 0 {
			logWarning("Wrote nothing to %s at offset %d.", path, offset)
			return false
		}
		logVerbose("Wrote %d to %s at offset %d.", wdone, path, offset)
		if wdone < rdone {
			logWarning("Short write to %s at offset %d (wrote %d instead of %d).", path, offset, wdone, rdone)
		}

		offset += int64(wdone)
	}

	atime, mtime, ok := statTimes(&sb)
	if !ok {
		logWarning("Unable to restore access and modification times on %s: unsupported stat timestamp fields.", path)
		return false
	}
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(syscall.TimespecToNsec(atime)),
		syscall.NsecToTimeval(syscall.TimespecToNsec(mtime)),
	}
	if err := syscall.Futimes(fd, tv); err != nil {
		logWarningWithError(err, "Unable to restore access and modification times on %s", path)
		return false
	}
	logVerbose("Restored access and modification times on %s.", path)

	ret = true
	return ret
}

func main() {
	flag.BoolVar(&verbose, "v", false, "enable verbose output")
	flag.BoolVar(&verbose, "verbose", false, "enable verbose output")
	flag.IntVar(&bufferSizeMB, "b", 8, "buffer size in MB")
	flag.IntVar(&bufferSizeMB, "buffersize", 8, "buffer size in MB")
	help := false
	flag.BoolVar(&help, "h", false, "show help")
	flag.BoolVar(&help, "help", false, "show help")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "  filerewrite [flags] file ...")
		flag.PrintDefaults()
	}

	flag.Parse()
	if help {
		flag.Usage()
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	if bufferSizeMB <= 0 {
		logWarning("invalid buffer size %d MB: must be greater than 0", bufferSizeMB)
		os.Exit(2)
	}

	bufferSizeBytes := bufferSizeMB * 1024 * 1024

	ret := 0
	for _, path := range args {
		logVerbose("Rewriting %s...", path)
		if !rewriteFile(path, bufferSizeBytes) {
			ret = 1
		}
	}
	os.Exit(ret)
}
