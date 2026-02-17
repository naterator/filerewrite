package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
