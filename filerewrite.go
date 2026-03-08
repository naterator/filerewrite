package main

import (
	"fmt"
	"log"
	"os"
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
	lstatFile = func(path string, sb *syscall.Stat_t) error {
		return syscall.Lstat(path, sb)
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

type pathOutcome int

const (
	pathOutcomeFailed pathOutcome = iota
	pathOutcomeRejectedNonRegular
	pathOutcomeSkippedHardlink
	pathOutcomeWouldRewrite
	pathOutcomeRewritten
)

type processOptions struct {
	bufferSizeBytes int
	dryRun          bool
	dedupHardlinks  bool
}

type pathResult struct {
	path           string
	outcome        pathOutcome
	bytesRewritten int64
}

type runStats struct {
	paths             int
	rewritten         int
	wouldRewrite      int
	skippedNonRegular int
	skippedHardlinks  int
	failures          int
	bytesRewritten    int64
}

type hardLinkKey struct {
	dev uint64
	ino uint64
}

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

func logInfo(format string, args ...any) {
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

func isRegularFile(mode uint32) bool {
	return (mode & syscall.S_IFMT) == syscall.S_IFREG
}

func hardLinkKeyFromStat(sb *syscall.Stat_t) hardLinkKey {
	return hardLinkKey{
		dev: uint64(sb.Dev),
		ino: uint64(sb.Ino),
	}
}

func trackHardLink(path string, sb *syscall.Stat_t, seen map[hardLinkKey]string) (string, bool) {
	if seen == nil {
		return "", false
	}

	key := hardLinkKeyFromStat(sb)
	if firstPath, ok := seen[key]; ok {
		return firstPath, true
	}

	seen[key] = path
	return "", false
}

func rewriteOpenFile(fd int, path string, bufferSizeBytes int, sb *syscall.Stat_t) pathResult {
	if bufferSizeBytes <= 0 {
		logWarning("invalid rewrite buffer size %d bytes: must be greater than 0", bufferSizeBytes)
		return pathResult{path: path, outcome: pathOutcomeFailed}
	}

	buf := make([]byte, bufferSizeBytes)

	var offset int64
	for {
		rdone, err := preadFile(fd, buf, offset)
		if err != nil {
			logWarningWithError(err, "Read from %s at offset %d failed", path, offset)
			return pathResult{path: path, outcome: pathOutcomeFailed}
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
				return pathResult{path: path, outcome: pathOutcomeFailed}
			}
			if wdone == 0 {
				logWarning("Wrote nothing to %s at offset %d.", path, writeOffset)
				return pathResult{path: path, outcome: pathOutcomeFailed}
			}
			logVerbose("Wrote %d to %s at offset %d.", wdone, path, writeOffset)
			if wdone < remaining {
				logWarning("Short write to %s at offset %d (wrote %d instead of %d).", path, writeOffset, wdone, remaining)
			}

			written += wdone
		}

		offset += int64(rdone)
	}

	atime, mtime, ok := statTimes(sb)
	if !ok {
		logWarning("Unable to restore access and modification times on %s: unsupported stat timestamp fields.", path)
		return pathResult{path: path, outcome: pathOutcomeFailed}
	}
	if err := restoreFileTimes(fd, atime, mtime); err != nil {
		logWarningWithError(err, "Unable to restore access and modification times on %s", path)
		return pathResult{path: path, outcome: pathOutcomeFailed}
	}
	logVerbose("Restored access and modification times on %s.", path)

	return pathResult{
		path:           path,
		outcome:        pathOutcomeRewritten,
		bytesRewritten: offset,
	}
}

func inspectPath(path string) (syscall.Stat_t, pathResult, bool) {
	var sb syscall.Stat_t
	if err := lstatFile(path, &sb); err != nil {
		logWarningWithError(err, "Unable to stat %s", path)
		return syscall.Stat_t{}, pathResult{path: path, outcome: pathOutcomeFailed}, false
	}
	if !isRegularFile(uint32(sb.Mode)) {
		logWarning("%s is not a regular file, skipping.", path)
		return syscall.Stat_t{}, pathResult{path: path, outcome: pathOutcomeRejectedNonRegular}, false
	}

	return sb, pathResult{}, true
}

func processPath(path string, options processOptions, seen map[hardLinkKey]string) pathResult {
	initialSB, result, ok := inspectPath(path)
	if !ok {
		return result
	}

	if options.dryRun {
		if options.dedupHardlinks {
			if firstPath, duplicate := trackHardLink(path, &initialSB, seen); duplicate {
				logInfo("WOULD SKIP HARDLINK %s (same inode as %s)", path, firstPath)
				return pathResult{path: path, outcome: pathOutcomeSkippedHardlink}
			}
		}

		logInfo("WOULD REWRITE %s", path)
		return pathResult{path: path, outcome: pathOutcomeWouldRewrite}
	}

	fd, err := openFile(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		logWarningWithError(err, "Unable to open %s", path)
		return pathResult{path: path, outcome: pathOutcomeFailed}
	}
	defer closeFile(fd)

	var openSB syscall.Stat_t
	if err := fstatFile(fd, &openSB); err != nil {
		logWarningWithError(err, "Unable to stat %s", path)
		return pathResult{path: path, outcome: pathOutcomeFailed}
	}
	if !isRegularFile(uint32(openSB.Mode)) {
		logWarning("%s is not a regular file, skipping.", path)
		return pathResult{path: path, outcome: pathOutcomeRejectedNonRegular}
	}

	if options.dedupHardlinks {
		if firstPath, duplicate := trackHardLink(path, &openSB, seen); duplicate {
			logVerbose("Skipping hard-link duplicate %s (same inode as %s).", path, firstPath)
			return pathResult{path: path, outcome: pathOutcomeSkippedHardlink}
		}
	}

	return rewriteOpenFile(fd, path, options.bufferSizeBytes, &openSB)
}

func (stats *runStats) add(result pathResult) {
	stats.paths++

	switch result.outcome {
	case pathOutcomeRewritten:
		stats.rewritten++
		stats.bytesRewritten += result.bytesRewritten
	case pathOutcomeWouldRewrite:
		stats.wouldRewrite++
	case pathOutcomeSkippedHardlink:
		stats.skippedHardlinks++
	case pathOutcomeRejectedNonRegular:
		stats.skippedNonRegular++
		stats.failures++
	case pathOutcomeFailed:
		stats.failures++
	}
}

func (stats runStats) summaryLine() string {
	return fmt.Sprintf(
		"Summary: paths=%d rewritten=%d would_rewrite=%d skipped_non_regular=%d skipped_hardlinks=%d failures=%d bytes_rewritten=%d",
		stats.paths,
		stats.rewritten,
		stats.wouldRewrite,
		stats.skippedNonRegular,
		stats.skippedHardlinks,
		stats.failures,
		stats.bytesRewritten,
	)
}

func rewriteFile(path string, bufferSizeBytes int) bool {
	return processPath(path, processOptions{bufferSizeBytes: bufferSizeBytes}, nil).outcome == pathOutcomeRewritten
}

func main() {
	flag.BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	flag.IntVarP(&bufferSizeMB, "buffersize", "b", 8, "buffer size in MB")
	dryRun := false
	flag.BoolVarP(&dryRun, "dry-run", "n", false, "report files that would be rewritten without modifying them")
	stats := false
	flag.BoolVar(&stats, "stats", false, "print summary statistics after processing")
	dedupHardlinks := false
	flag.BoolVar(&dedupHardlinks, "dedup-hardlinks", false, "skip duplicate hard-linked files within a single run")
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

	options := processOptions{
		bufferSizeBytes: bufferSizeBytes,
		dryRun:          dryRun,
		dedupHardlinks:  dedupHardlinks,
	}
	seenHardLinks := make(map[hardLinkKey]string)
	run := runStats{}

	ret := 0
	for _, path := range args {
		if options.dryRun {
			logVerbose("Inspecting %s...", path)
		} else {
			logVerbose("Rewriting %s...", path)
		}

		result := processPath(path, options, seenHardLinks)
		run.add(result)
		if result.outcome == pathOutcomeFailed || result.outcome == pathOutcomeRejectedNonRegular {
			ret = 1
		}
	}

	if stats {
		logInfo("%s", run.summaryLine())
	}

	os.Exit(ret)
}
