package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	flag "github.com/spf13/pflag"
)

func TestMain(m *testing.M) {
	log.SetFlags(0)
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	cmdArgs := append([]string{"-test.run=TestCLIMainHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}

	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run helper process: %v", err)
	}
	return exitErr.ExitCode(), stdout.String(), stderr.String()
}

func TestCLIMainHelper(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}

	log.SetFlags(0)
	log.SetOutput(os.Stderr)

	args := []string{"filerewrite"}
	for i := 0; i < len(os.Args); i++ {
		if os.Args[i] == "--" {
			args = append(args, os.Args[i+1:]...)
			break
		}
	}
	os.Args = args
	main()
}

func fileTimes(t *testing.T, path string) (syscall.Timespec, syscall.Timespec) {
	t.Helper()

	var sb syscall.Stat_t
	if err := syscall.Stat(path, &sb); err != nil {
		t.Fatalf("stat(%q): %v", path, err)
	}

	atime, mtime, ok := statTimes(&sb)
	if !ok {
		t.Fatalf("unsupported stat timestamp fields")
	}
	return atime, mtime
}

func TestNormalizeGoStyleLongFlags(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.Bool("verbose", false, "")
	fs.Int("buffersize", 8, "")

	got := normalizeGoStyleLongFlags([]string{
		"-buffersize=1",
		"-verbose=false",
		"--verbose",
		"-unknown",
		"--",
		"-file",
		"-",
		"plain",
	}, fs)

	want := []string{
		"--buffersize=1",
		"--verbose=false",
		"--verbose",
		"-unknown",
		"--",
		"-file",
		"-",
		"plain",
	}

	if len(got) != len(want) {
		t.Fatalf("normalizeGoStyleLongFlags length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeGoStyleLongFlags[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRewriteFilePreservesDataAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("filerewrite-test-data-"), 2048)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	atimeSet := time.Unix(1700000000, 123000000)
	mtimeSet := time.Unix(1700000100, 456000000)
	if err := os.Chtimes(path, atimeSet, mtimeSet); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	expectedAtime, expectedMtime := fileTimes(t, path)

	if ok := rewriteFile(path, 7); !ok {
		t.Fatalf("rewriteFile returned false")
	}

	gotAtime, gotMtime := fileTimes(t, path)
	if syscall.TimespecToNsec(gotAtime) != syscall.TimespecToNsec(expectedAtime) {
		t.Fatalf("atime changed: got=%d want=%d", syscall.TimespecToNsec(gotAtime), syscall.TimespecToNsec(expectedAtime))
	}
	if syscall.TimespecToNsec(gotMtime) != syscall.TimespecToNsec(expectedMtime) {
		t.Fatalf("mtime changed: got=%d want=%d", syscall.TimespecToNsec(gotMtime), syscall.TimespecToNsec(expectedMtime))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("rewritten file data changed")
	}
}

func TestRewriteFileCompletesShortWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("short-write-regression-"), 32)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	originalPwrite := pwriteFile
	pwriteFile = func(fd int, buf []byte, offset int64) (int, error) {
		if len(buf) > 3 {
			buf = buf[:3]
		}
		return originalPwrite(fd, buf, offset)
	}
	t.Cleanup(func() {
		pwriteFile = originalPwrite
	})

	if ok := rewriteFile(path, 11); !ok {
		t.Fatalf("rewriteFile returned false")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rewritten file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("rewritten file data changed after short writes")
	}
}

func TestRewriteFileRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if ok := rewriteFile(dir, 1024); ok {
		t.Fatalf("rewriteFile(directory) = true, want false")
	}
}

func TestRewriteFileRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	link := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if ok := rewriteFile(link, 1024); ok {
		t.Fatalf("rewriteFile(symlink) = true, want false")
	}
}

func TestRewriteFileMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.txt")
	if ok := rewriteFile(path, 1024); ok {
		t.Fatalf("rewriteFile(missing file) = true, want false")
	}
}

func TestCLIHelpShortFlag(t *testing.T) {
	exitCode, _, stderr := runCLI(t, "-h")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stderr, "Usage of filerewrite:") {
		t.Fatalf("help output missing usage header: %q", stderr)
	}
	if !strings.Contains(stderr, "-b, -buffersize int") {
		t.Fatalf("help output missing buffersize flag: %q", stderr)
	}
	if strings.Contains(stderr, "--buffersize") {
		t.Fatalf("help output should not use double-dash long flags: %q", stderr)
	}
	if !strings.Contains(stderr, "-n, -dry-run") {
		t.Fatalf("help output missing dry-run flag: %q", stderr)
	}
	if !strings.Contains(stderr, "-dedup-hardlinks") {
		t.Fatalf("help output missing dedup-hardlinks flag: %q", stderr)
	}
	if !strings.Contains(stderr, "-stats") {
		t.Fatalf("help output missing stats flag: %q", stderr)
	}
}

func TestCLIHelpLongFlag(t *testing.T) {
	exitCode, _, stderr := runCLI(t, "-help")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0", exitCode)
	}
	if !strings.Contains(stderr, "Usage of filerewrite:") {
		t.Fatalf("help output missing usage header: %q", stderr)
	}
}

func TestCLINoArgsExitsWithUsage(t *testing.T) {
	exitCode, _, stderr := runCLI(t)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr, "Usage of filerewrite:") {
		t.Fatalf("usage output missing: %q", stderr)
	}
}

func TestCLIVerboseShortFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-v", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Rewriting "+path+"...") {
		t.Fatalf("verbose output missing rewrite line: %q", stderr)
	}
}

func TestCLIVerboseLongFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-verbose", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Rewriting "+path+"...") {
		t.Fatalf("verbose output missing rewrite line: %q", stderr)
	}
}

func TestCLIBufferSizeShortFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-b", "1", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
}

func TestCLIBufferSizeLongFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-buffersize", "1", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
}

func TestCLIBufferSizeLongFlagWithEquals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-buffersize=1", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
}

func TestCLILongFlagsStillSupportDoubleDash(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--buffersize", "1", "--verbose", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Rewriting "+path+"...") {
		t.Fatalf("verbose output missing rewrite line: %q", stderr)
	}
}

func TestCLIBooleanLongFlagSupportsExplicitFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-verbose=false", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if strings.Contains(stderr, "Rewriting "+path+"...") {
		t.Fatalf("verbose output should be disabled, got: %q", stderr)
	}
}

func TestCLIEndOfFlagsAllowsDashPrefixedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "-data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-v", "--", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Rewriting "+path+"...") {
		t.Fatalf("verbose output missing rewrite line: %q", stderr)
	}
}

func TestCLIInvalidBufferSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "-b", "0", path)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "invalid buffer size 0 MB") {
		t.Fatalf("expected invalid buffer size warning, got: %q", stderr)
	}
}

func TestCLIDryRunReportsWithoutChangingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	original := bytes.Repeat([]byte("dry-run-test-"), 32)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	atimeSet := time.Unix(1700001000, 111000000)
	mtimeSet := time.Unix(1700002000, 222000000)
	if err := os.Chtimes(path, atimeSet, mtimeSet); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	expectedAtime, expectedMtime := fileTimes(t, path)

	exitCode, _, stderr := runCLI(t, "--dry-run", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "WOULD REWRITE "+path) {
		t.Fatalf("dry-run output missing rewrite report: %q", stderr)
	}

	gotAtime, gotMtime := fileTimes(t, path)
	if syscall.TimespecToNsec(gotAtime) != syscall.TimespecToNsec(expectedAtime) {
		t.Fatalf("atime changed in dry-run: got=%d want=%d", syscall.TimespecToNsec(gotAtime), syscall.TimespecToNsec(expectedAtime))
	}
	if syscall.TimespecToNsec(gotMtime) != syscall.TimespecToNsec(expectedMtime) {
		t.Fatalf("mtime changed in dry-run: got=%d want=%d", syscall.TimespecToNsec(gotMtime), syscall.TimespecToNsec(expectedMtime))
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file after dry-run: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("dry-run changed file data")
	}
}

func TestCLIDryRunFailurePathsExitOne(t *testing.T) {
	dir := t.TempDir()
	regularPath := filepath.Join(dir, "data.txt")
	missingPath := filepath.Join(dir, "missing.txt")
	symlinkPath := filepath.Join(dir, "link.txt")
	if err := os.WriteFile(regularPath, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Symlink(regularPath, symlinkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--dry-run", regularPath, missingPath, symlinkPath)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "WOULD REWRITE "+regularPath) {
		t.Fatalf("dry-run output missing regular file report: %q", stderr)
	}
	if !strings.Contains(stderr, missingPath) {
		t.Fatalf("dry-run output missing missing-file warning: %q", stderr)
	}
	if !strings.Contains(stderr, symlinkPath+" is not a regular file, skipping.") {
		t.Fatalf("dry-run output missing non-regular warning: %q", stderr)
	}
}

func TestCLIOversizedBufferSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	maxIntValue := int(^uint(0) >> 1)
	tooLarge := maxIntValue/bytesPerMB + 1

	exitCode, _, stderr := runCLI(t, "-b", strconv.Itoa(tooLarge), path)
	if exitCode != 2 {
		t.Fatalf("exit code = %d, want 2; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "exceeds platform limit") {
		t.Fatalf("expected oversized buffer size warning, got: %q", stderr)
	}
}

func TestCLIStatsSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--stats", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Summary: paths=1 rewritten=1 would_rewrite=0 skipped_non_regular=0 skipped_hardlinks=0 failures=0 bytes_rewritten=3") {
		t.Fatalf("stats summary missing or incorrect: %q", stderr)
	}
}

func TestCLIDedupHardlinksDryRunWithStats(t *testing.T) {
	dir := t.TempDir()
	primaryPath := filepath.Join(dir, "primary.txt")
	duplicatePath := filepath.Join(dir, "duplicate.txt")
	if err := os.WriteFile(primaryPath, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Link(primaryPath, duplicatePath); err != nil {
		t.Fatalf("create hard link: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--dry-run", "--dedup-hardlinks", "--stats", primaryPath, duplicatePath)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "WOULD REWRITE "+primaryPath) {
		t.Fatalf("dry-run output missing primary rewrite report: %q", stderr)
	}
	if !strings.Contains(stderr, "WOULD SKIP HARDLINK "+duplicatePath) {
		t.Fatalf("dry-run output missing hard-link skip report: %q", stderr)
	}
	if !strings.Contains(stderr, "Summary: paths=2 rewritten=0 would_rewrite=1 skipped_non_regular=0 skipped_hardlinks=1 failures=0 bytes_rewritten=0") {
		t.Fatalf("stats summary missing or incorrect: %q", stderr)
	}
}

func TestCLIDedupHardlinksStats(t *testing.T) {
	dir := t.TempDir()
	primaryPath := filepath.Join(dir, "primary.txt")
	duplicatePath := filepath.Join(dir, "duplicate.txt")
	if err := os.WriteFile(primaryPath, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Link(primaryPath, duplicatePath); err != nil {
		t.Fatalf("create hard link: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--dedup-hardlinks", "--stats", primaryPath, duplicatePath)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Summary: paths=2 rewritten=1 would_rewrite=0 skipped_non_regular=0 skipped_hardlinks=1 failures=0 bytes_rewritten=3") {
		t.Fatalf("stats summary missing or incorrect: %q", stderr)
	}
}

func TestCLIMixedResultsExitOne(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "data.txt")
	missingPath := filepath.Join(dir, "missing.txt")
	original := []byte("abc")
	if err := os.WriteFile(validPath, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, validPath, missingPath)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, missingPath) {
		t.Fatalf("missing file warning not reported: %q", stderr)
	}

	got, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatalf("read valid file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("valid file data changed")
	}
}

func TestCLIStatsMixedResults(t *testing.T) {
	dir := t.TempDir()
	validPath := filepath.Join(dir, "data.txt")
	missingPath := filepath.Join(dir, "missing.txt")
	if err := os.WriteFile(validPath, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--stats", validPath, missingPath)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%q", exitCode, stderr)
	}
	if !strings.Contains(stderr, "Summary: paths=2 rewritten=1 would_rewrite=0 skipped_non_regular=0 skipped_hardlinks=0 failures=1 bytes_rewritten=3") {
		t.Fatalf("stats summary missing or incorrect: %q", stderr)
	}
}
