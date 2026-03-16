//go:build linux || darwin || freebsd || netbsd || openbsd

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type stubReleaseUpdater struct {
	runCalls       int
	currentVersion string
	err            error
}

func (s *stubReleaseUpdater) Run(ctx context.Context, currentVersion string, stdout io.Writer) error {
	s.runCalls++
	s.currentVersion = currentVersion
	if stdout != nil {
		_, _ = io.WriteString(stdout, "stub updater invoked\n")
	}
	return s.err
}

func withReleaseUpdaterStub(t *testing.T, updater releaseUpdater) {
	t.Helper()
	previous := makeReleaseUpdater
	makeReleaseUpdater = func() releaseUpdater { return updater }
	t.Cleanup(func() {
		makeReleaseUpdater = previous
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newTestClient(fn roundTripFunc) *http.Client {
	return &http.Client{Transport: fn}
}

func testResponse(req *http.Request, statusCode int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     http.StatusText(statusCode),
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

func TestCLIAutoupdateRunsUpdater(t *testing.T) {
	stub := &stubReleaseUpdater{}
	withReleaseUpdaterStub(t, stub)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"-autoupdate", "-buffersize=0", "ignored-path"}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stub.runCalls != 1 {
		t.Fatalf("stub updater runCalls = %d, want 1", stub.runCalls)
	}
	if stub.currentVersion != appVersion {
		t.Fatalf("stub updater currentVersion = %q, want %q", stub.currentVersion, appVersion)
	}
	if !strings.Contains(stdout.String(), "stub updater invoked") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestCLIAutoupdateReportsUpdaterError(t *testing.T) {
	stub := &stubReleaseUpdater{err: io.EOF}
	withReleaseUpdaterStub(t, stub)

	var stdout bytes.Buffer
	var stderr bytes.Buffer

	exitCode := run([]string{"-autoupdate"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	if stub.runCalls != 1 {
		t.Fatalf("stub updater runCalls = %d, want 1", stub.runCalls)
	}
	if !strings.Contains(stderr.String(), "autoupdate failed: EOF") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestGitHubReleaseUpdaterReplacesExecutable(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), appName)
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	binaryCandidates, checksumCandidates := releaseAssetCandidates(runtime.GOOS, runtime.GOARCH)
	binaryName := binaryCandidates[0]
	checksumName := checksumCandidates[0]
	binaryBody := []byte("new-binary-content")
	sum := sha256.Sum256(binaryBody)
	checksumBody := hex.EncodeToString(sum[:]) + "  " + binaryName + "\n"

	var metadataCalls int
	var binaryCalls int
	var checksumCalls int
	baseURL := "https://example.test"
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/repos/naterator/filerewrite/releases/latest":
			metadataCalls++
			if got := r.Header.Get("User-Agent"); got != appName+"/"+appVersion {
				t.Fatalf("User-Agent = %q", got)
			}
			body, err := json.Marshal(githubRelease{
				TagName: "v9.9.9",
				Assets: []githubReleaseAsset{
					{Name: binaryName, BrowserDownloadURL: baseURL + "/download/" + binaryName},
					{Name: checksumName, BrowserDownloadURL: baseURL + "/download/" + checksumName},
				},
			})
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			return testResponse(r, http.StatusOK, body), nil
		case "/download/" + binaryName:
			binaryCalls++
			return testResponse(r, http.StatusOK, binaryBody), nil
		case "/download/" + checksumName:
			checksumCalls++
			return testResponse(r, http.StatusOK, []byte(checksumBody)), nil
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return nil, nil
		}
	})

	updater := &githubReleaseUpdater{
		client:           client,
		latestReleaseURL: baseURL + "/repos/naterator/filerewrite/releases/latest",
		executablePath: func() (string, error) {
			return exePath, nil
		},
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}

	var stdout bytes.Buffer
	if err := updater.Run(context.Background(), "1.0.0", &stdout); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(got) != string(binaryBody) {
		t.Fatalf("updated executable content = %q, want %q", string(got), string(binaryBody))
	}
	info, err := os.Stat(exePath)
	if err != nil {
		t.Fatalf("Stat returned error: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("updated executable mode = %#o, want 0755", info.Mode().Perm())
	}
	if metadataCalls != 1 || binaryCalls != 1 || checksumCalls != 1 {
		t.Fatalf("calls = metadata:%d binary:%d checksum:%d", metadataCalls, binaryCalls, checksumCalls)
	}
	if !strings.Contains(stdout.String(), "Updating "+appName+" from 1.0.0 to v9.9.9") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Updated "+appName+" to v9.9.9") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestGitHubReleaseUpdaterSkipsCurrentVersion(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), appName)
	if err := os.WriteFile(exePath, []byte("existing-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	var metadataCalls int
	var downloadCalls int
	baseURL := "https://example.test"
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/repos/naterator/filerewrite/releases/latest":
			metadataCalls++
			body, err := json.Marshal(githubRelease{
				TagName: "v1.4.1",
				Assets:  []githubReleaseAsset{},
			})
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			return testResponse(r, http.StatusOK, body), nil
		default:
			downloadCalls++
			t.Fatalf("unexpected download path: %s", r.URL.Path)
			return nil, nil
		}
	})

	updater := &githubReleaseUpdater{
		client:           client,
		latestReleaseURL: baseURL + "/repos/naterator/filerewrite/releases/latest",
		executablePath: func() (string, error) {
			return exePath, nil
		},
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}

	var stdout bytes.Buffer
	if err := updater.Run(context.Background(), "1.4.1", &stdout); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}
	if string(got) != "existing-binary" {
		t.Fatalf("executable content = %q, want existing-binary", string(got))
	}
	if metadataCalls != 1 || downloadCalls != 0 {
		t.Fatalf("calls = metadata:%d download:%d", metadataCalls, downloadCalls)
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestGitHubReleaseUpdaterRejectsChecksumMismatch(t *testing.T) {
	exePath := filepath.Join(t.TempDir(), appName)
	if err := os.WriteFile(exePath, []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	binaryCandidates, checksumCandidates := releaseAssetCandidates(runtime.GOOS, runtime.GOARCH)
	binaryName := binaryCandidates[0]
	checksumName := checksumCandidates[0]

	baseURL := "https://example.test"
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/repos/naterator/filerewrite/releases/latest":
			body, err := json.Marshal(githubRelease{
				TagName: "v9.9.9",
				Assets: []githubReleaseAsset{
					{Name: binaryName, BrowserDownloadURL: baseURL + "/download/" + binaryName},
					{Name: checksumName, BrowserDownloadURL: baseURL + "/download/" + checksumName},
				},
			})
			if err != nil {
				t.Fatalf("Marshal returned error: %v", err)
			}
			return testResponse(r, http.StatusOK, body), nil
		case "/download/" + binaryName:
			return testResponse(r, http.StatusOK, []byte("tampered-binary")), nil
		case "/download/" + checksumName:
			return testResponse(r, http.StatusOK, []byte(strings.Repeat("a", 64)+"  "+binaryName+"\n")), nil
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
			return nil, nil
		}
	})

	updater := &githubReleaseUpdater{
		client:           client,
		latestReleaseURL: baseURL + "/repos/naterator/filerewrite/releases/latest",
		executablePath: func() (string, error) {
			return exePath, nil
		},
		goos:   runtime.GOOS,
		goarch: runtime.GOARCH,
	}

	err := updater.Run(context.Background(), "1.0.0", io.Discard)
	if err == nil {
		t.Fatal("Run unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Run error = %v", err)
	}

	got, readErr := os.ReadFile(exePath)
	if readErr != nil {
		t.Fatalf("ReadFile returned error: %v", readErr)
	}
	if string(got) != "old-binary" {
		t.Fatalf("executable content = %q, want old-binary", string(got))
	}
}

func TestParseSHA256FilePrefersNamedAsset(t *testing.T) {
	checksum, err := parseSHA256File(strings.Join([]string{
		strings.Repeat("a", 64) + "  other",
		strings.Repeat("b", 64) + "  wanted",
	}, "\n"), "wanted")
	if err != nil {
		t.Fatalf("parseSHA256File returned error: %v", err)
	}
	if checksum != strings.Repeat("b", 64) {
		t.Fatalf("checksum = %q, want %q", checksum, strings.Repeat("b", 64))
	}
}

func TestParseSHA256FileRejectsMultipleUnnamedDigests(t *testing.T) {
	_, err := parseSHA256File(strings.Join([]string{
		"SHA256(file1)= " + strings.Repeat("a", 64),
		"SHA256(file2)= " + strings.Repeat("b", 64),
	}, "\n"), "wanted")
	if err == nil {
		t.Fatal("parseSHA256File unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "multiple unnamed digests") {
		t.Fatalf("error = %v", err)
	}
}

func TestNormalizeSemver(t *testing.T) {
	testCases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "plain", input: "1.2.3", want: "v1.2.3"},
		{name: "prefixed", input: "v1.2.3", want: "v1.2.3"},
		{name: "missing patch", input: "1.2", wantErr: true},
		{name: "prerelease", input: "v1.2.3-rc1", wantErr: true},
		{name: "empty", input: "", wantErr: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeSemver(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeSemver(%q) unexpectedly succeeded", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeSemver(%q) returned error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeSemver(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
