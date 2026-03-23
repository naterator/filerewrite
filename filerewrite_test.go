//go:build linux || darwin || freebsd || netbsd || openbsd

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	return runCLIInDir(t, "", args...)
}

func runCLIInDir(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()

	cmdArgs := append([]string{"-test.run=TestCLIMainHelper", "--"}, args...)
	cmd := exec.Command(os.Args[0], cmdArgs...)
	cmd.Env = append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	if dir != "" {
		cmd.Dir = dir
	}

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

func TestRewriteFilePreservesDataAndTimestamps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("filerewrite-test-data-"), 2048)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	atimeSet := time.Unix(1700000000, 123456789)
	mtimeSet := time.Unix(1700000100, 456789123)
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

func TestRewriteFileEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.bin")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if ok := rewriteFile(path, 1024); !ok {
		t.Fatalf("rewriteFile(empty) returned false")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty file now has %d bytes", len(got))
	}
}

func TestRewriteFileExactBufferBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "boundary.bin")

	bufSize := 256
	original := bytes.Repeat([]byte("x"), bufSize)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	if ok := rewriteFile(path, bufSize); !ok {
		t.Fatalf("rewriteFile returned false")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("file data changed at exact buffer boundary")
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

// TestRewritePreservesContentAcrossSizes exercises the pread/pwrite loop
// across many file-size and buffer-size combinations to verify that the
// file is byte-for-byte identical after rewrite. This is the core safety
// property of the tool.
func TestRewritePreservesContentAcrossSizes(t *testing.T) {
	type testCase struct {
		size int
		buf  int
	}
	var cases []testCase
	for _, size := range []int{1, 2, 63, 64, 65, 127, 128, 129, 1023, 1024, 1025, 4095, 4096, 4097} {
		for _, buf := range []int{1, 7, 64, 128, 1024, 4096, 8192} {
			cases = append(cases, testCase{size, buf})
		}
	}
	// One larger file to exercise sustained I/O.
	cases = append(cases, testCase{1024 * 1024, 8192})

	for _, tc := range cases {
		t.Run(fmt.Sprintf("size=%d_buf=%d", tc.size, tc.buf), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "data.bin")

			// Deterministic content using a prime modulus to cover all byte
			// values and avoid alignment patterns.
			original := make([]byte, tc.size)
			for i := range original {
				original[i] = byte(i % 251)
			}

			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}

			if ok := rewriteFile(path, tc.buf); !ok {
				t.Fatalf("rewriteFile returned false")
			}

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read file: %v", err)
			}
			if !bytes.Equal(got, original) {
				t.Fatalf("file content changed (size=%d buf=%d)", tc.size, tc.buf)
			}
		})
	}
}

// TestRewriteContentUnchangedOnReadError verifies that if pread fails
// mid-file, the bytes already written back are the original data and the
// rest of the file is untouched.
func TestRewriteContentUnchangedOnReadError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("important-data-"), 200)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	readCount := 0
	savedPread := preadFile
	preadFile = func(fd int, buf []byte, offset int64) (int, error) {
		readCount++
		if readCount == 3 {
			return 0, syscall.EIO
		}
		return savedPread(fd, buf, offset)
	}
	t.Cleanup(func() { preadFile = savedPread })

	if ok := rewriteFile(path, 64); ok {
		t.Fatalf("rewriteFile should have failed on injected read error")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("file content changed after read error")
	}
}

// TestRewriteContentUnchangedOnWriteError verifies that if pwrite fails
// mid-file, the file is still byte-for-byte identical because every
// successful write wrote back the original bytes.
func TestRewriteContentUnchangedOnWriteError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("important-data-"), 200)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	writeCount := 0
	savedPwrite := pwriteFile
	pwriteFile = func(fd int, buf []byte, offset int64) (int, error) {
		writeCount++
		if writeCount == 3 {
			return 0, syscall.EIO
		}
		return savedPwrite(fd, buf, offset)
	}
	t.Cleanup(func() { pwriteFile = savedPwrite })

	if ok := rewriteFile(path, 64); ok {
		t.Fatalf("rewriteFile should have failed on injected write error")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("file content changed after write error")
	}
}

// TestRewriteContentUnchangedOnZeroWrite verifies that if pwrite returns
// (0, nil) — no progress — the rewrite bails out rather than looping
// forever, and the file content is unchanged.
func TestRewriteContentUnchangedOnZeroWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")

	original := bytes.Repeat([]byte("important-data-"), 200)
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	writeCount := 0
	savedPwrite := pwriteFile
	pwriteFile = func(fd int, buf []byte, offset int64) (int, error) {
		writeCount++
		if writeCount == 2 {
			return 0, nil
		}
		return savedPwrite(fd, buf, offset)
	}
	t.Cleanup(func() { pwriteFile = savedPwrite })

	if ok := rewriteFile(path, 64); ok {
		t.Fatalf("rewriteFile should have failed on zero-length write")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("file content changed after zero-length write")
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
	if !strings.Contains(stderr, "-b, --buffersize int") {
		t.Fatalf("help output missing buffersize flag: %q", stderr)
	}
	if !strings.Contains(stderr, "-n, --dry-run") {
		t.Fatalf("help output missing dry-run flag: %q", stderr)
	}
	if !strings.Contains(stderr, "--selfupdate") {
		t.Fatalf("help output missing selfupdate flag: %q", stderr)
	}
	if !strings.Contains(stderr, "--dedup-hardlinks") {
		t.Fatalf("help output missing dedup-hardlinks flag: %q", stderr)
	}
	if !strings.Contains(stderr, "--stats") {
		t.Fatalf("help output missing stats flag: %q", stderr)
	}
	if !strings.Contains(stderr, "--version") {
		t.Fatalf("help output missing version flag: %q", stderr)
	}
}

func TestCLIHelpLongFlag(t *testing.T) {
	exitCode, _, stderr := runCLI(t, "--help")
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

func TestCLIVersionFlag(t *testing.T) {
	exitCode, stdout, stderr := runCLI(t, "--version")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stdout != appVersion+"\n" {
		t.Fatalf("stdout = %q, want %q", stdout, appVersion+"\\n")
	}
	if stderr != "" {
		t.Fatalf("stderr = %q, want empty", stderr)
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

	exitCode, _, stderr := runCLI(t, "--verbose", path)
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

	exitCode, _, stderr := runCLI(t, "--buffersize", "1", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
}

func TestCLIBufferSizeLongFlagWithEquals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 4096), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLI(t, "--buffersize=1", path)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
}

func TestCLILongFlags(t *testing.T) {
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

	exitCode, _, stderr := runCLI(t, "--verbose=false", path)
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

func TestCLIEndOfFlagsAllowsFlagShapedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "-verbose")
	if err := os.WriteFile(path, []byte("abc"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	exitCode, _, stderr := runCLIInDir(t, dir, "--dry-run", "--", "-verbose")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr)
	}
	if stderr != "WOULD REWRITE -verbose\n" {
		t.Fatalf("stderr = %q, want %q", stderr, "WOULD REWRITE -verbose\\n")
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
	if stderr != "WOULD REWRITE "+path+"\n" {
		t.Fatalf("dry-run output = %q, want %q", stderr, "WOULD REWRITE "+path+"\\n")
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
