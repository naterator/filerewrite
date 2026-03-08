package main

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strings"
	"syscall"

	flag "github.com/spf13/pflag"
)

var verbose bool
var bufferSizeMB int

const bytesPerMB = 1024 * 1024

var (
	openFile = func(path string, mode int, perm uint32) (int, error) {
		return syscall.Open(path, mode, perm)
	}
	closeFile = func(fd int) error {
		return syscall.Close(fd)
	}
	fstatFile = func(fd int, sb *syscall.Stat_t) error {
		return syscall.Fstat(fd, sb)
	}
	preadFile = func(fd int, buf []byte, offset int64) (int, error) {
		return syscall.Pread(fd, buf, offset)
	}
	pwriteFile = func(fd int, buf []byte, offset int64) (int, error) {
		return syscall.Pwrite(fd, buf, offset)
	}
	futimesFile = func(fd int, tv []syscall.Timeval) error {
		return syscall.Futimes(fd, tv)
	}
)

func normalizeGoStyleLongFlags(args []string, fs *flag.FlagSet) []string {
	normalized := make([]string, 0, len(args))
	for _, arg := range args {
		// Keep literals and already-gnu-style options as-is.
		if !strings.HasPrefix(arg, "-") || strings.HasPrefix(arg, "--") || arg == "-" {
			normalized = append(normalized, arg)
			continue
		}
		if len(arg) <= 2 {
			normalized = append(normalized, arg)
			continue
		}

		name := strings.TrimPrefix(arg, "-")
		value := ""
		if idx := strings.Index(name, "="); idx >= 0 {
			value = name[idx:]
			name = name[:idx]
		}

		// If this matches a defined long flag name, promote to --long.
		if fs.Lookup(name) != nil {
			normalized = append(normalized, "--"+name+value)
			continue
		}

		normalized = append(normalized, arg)
	}
	return normalized
}

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

func bufferSizeBytesFromMB(sizeMB int) (int, error) {
	if sizeMB <= 0 {
		return 0, fmt.Errorf("invalid buffer size %d MB: must be greater than 0", sizeMB)
	}

	maxIntValue := int(^uint(0) >> 1)
	maxBufferSizeMB := maxIntValue / bytesPerMB
	if sizeMB > maxBufferSizeMB {
		return 0, fmt.Errorf("invalid buffer size %d MB: exceeds platform limit", sizeMB)
	}

	return sizeMB * bytesPerMB, nil
}

func rewriteFile(path string, bufferSizeBytes int) bool {
	if bufferSizeBytes <= 0 {
		logWarning("invalid rewrite buffer size %d bytes: must be greater than 0", bufferSizeBytes)
		return false
	}

	buf := make([]byte, bufferSizeBytes)
	ret := false

	fd, err := openFile(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		logWarningWithError(err, "Unable to open %s", path)
		return false
	}
	defer closeFile(fd)

	var sb syscall.Stat_t
	if err := fstatFile(fd, &sb); err != nil {
		logWarningWithError(err, "Unable to stat %s", path)
		return false
	}
	if (sb.Mode & syscall.S_IFMT) != syscall.S_IFREG {
		logWarning("%s is not a regular file, skipping.", path)
		return false
	}

	var offset int64
	for {
		rdone, err := preadFile(fd, buf, offset)
		if err != nil {
			logWarningWithError(err, "Read from %s at offset %d failed", path, offset)
			return false
		}
		if rdone == 0 {
			break
		}
		logVerbose("Read %d from %s at offset %d.", rdone, path, offset)

		written := 0
		for written < rdone {
			writeOffset := offset + int64(written)
			remaining := rdone - written

			wdone, err := pwriteFile(fd, buf[written:rdone], writeOffset)
			if err != nil {
				logWarningWithError(err, "Write %s at offset %d failed", path, writeOffset)
				return false
			}
			if wdone == 0 {
				logWarning("Wrote nothing to %s at offset %d.", path, writeOffset)
				return false
			}
			logVerbose("Wrote %d to %s at offset %d.", wdone, path, writeOffset)
			if wdone < remaining {
				logWarning("Short write to %s at offset %d (wrote %d instead of %d).", path, writeOffset, wdone, remaining)
			}

			written += wdone
		}

		offset += int64(rdone)
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
	if err := futimesFile(fd, tv); err != nil {
		logWarningWithError(err, "Unable to restore access and modification times on %s", path)
		return false
	}
	logVerbose("Restored access and modification times on %s.", path)

	ret = true
	return ret
}

func main() {
	flag.BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	flag.IntVarP(&bufferSizeMB, "buffersize", "b", 8, "buffer size in MB")
	help := false
	flag.BoolVarP(&help, "help", "h", false, "show help")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "Usage of %s:\n", os.Args[0])
		_, _ = fmt.Fprintln(flag.CommandLine.Output(), "  filerewrite [flags] file ...")
		flag.VisitAll(func(f *flag.Flag) {
			typeName := ""
			if f.Value.Type() != "bool" {
				typeName = " " + f.Value.Type()
			}

			flagLabel := fmt.Sprintf("-%s%s", f.Name, typeName)
			if f.Shorthand != "" {
				flagLabel = fmt.Sprintf("-%s, -%s%s", f.Shorthand, f.Name, typeName)
			}
			_, _ = fmt.Fprintf(flag.CommandLine.Output(), "  %-24s %s", flagLabel, f.Usage)
			if f.DefValue != "" && f.DefValue != "false" {
				_, _ = fmt.Fprintf(flag.CommandLine.Output(), " (default %s)", f.DefValue)
			}
			_, _ = fmt.Fprintln(flag.CommandLine.Output())
		})
	}

	normalizedArgs := normalizeGoStyleLongFlags(os.Args[1:], flag.CommandLine)
	if err := flag.CommandLine.Parse(normalizedArgs); err != nil {
		os.Exit(2)
	}
	if help {
		flag.Usage()
		os.Exit(0)
	}

	args := flag.Args()
	if len(args) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	bufferSizeBytes, err := bufferSizeBytesFromMB(bufferSizeMB)
	if err != nil {
		logWarning("%v", err)
		os.Exit(2)
	}

	ret := 0
	for _, path := range args {
		logVerbose("Rewriting %s...", path)
		if !rewriteFile(path, bufferSizeBytes) {
			ret = 1
		}
	}
	os.Exit(ret)
}
