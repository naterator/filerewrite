package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const githubLatestReleaseURL = "https://api.github.com/repos/naterator/filerewrite/releases/latest"

type releaseUpdater interface {
	Run(ctx context.Context, currentVersion string, stdout io.Writer) error
}

type githubReleaseUpdater struct {
	client           *http.Client
	latestReleaseURL string
	executablePath   func() (string, error)
	goos             string
	goarch           string
}

type githubRelease struct {
	TagName string               `json:"tag_name"`
	Assets  []githubReleaseAsset `json:"assets"`
}

type githubReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

var makeReleaseUpdater = newDefaultReleaseUpdater

func newDefaultReleaseUpdater() releaseUpdater {
	return &githubReleaseUpdater{
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
		latestReleaseURL: githubLatestReleaseURL,
		executablePath:   os.Executable,
		goos:             runtime.GOOS,
		goarch:           runtime.GOARCH,
	}
}

func (u *githubReleaseUpdater) Run(ctx context.Context, currentVersion string, stdout io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if stdout == nil {
		stdout = io.Discard
	}

	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return err
	}

	latestVersion, err := normalizeSemver(release.TagName)
	if err != nil {
		return fmt.Errorf("latest release tag %q is not valid semver: %w", release.TagName, err)
	}

	currentVersionText := currentVersion
	if currentVersionText == "" {
		currentVersionText = "unknown"
	}
	currentVersionNormalized, currentVersionKnown := normalizeSemverLoose(currentVersion)
	if currentVersionKnown {
		switch compareSemver(currentVersionNormalized, latestVersion) {
		case 1:
			fmt.Fprintf(stdout, "%s %s is newer than published release %s; skipping\n", appName, currentVersionText, latestVersion)
			return nil
		case 0:
			fmt.Fprintf(stdout, "%s %s is already up to date\n", appName, currentVersionText)
			return nil
		}
	}

	binaryAsset, checksumAsset, err := release.assetsForRuntime(u.goos, u.goarch)
	if err != nil {
		return err
	}

	exePath, err := u.executablePath()
	if err != nil {
		return fmt.Errorf("locate current executable: %w", err)
	}
	if resolvedPath, resolveErr := filepath.EvalSymlinks(exePath); resolveErr == nil {
		exePath = resolvedPath
	}

	expectedChecksum, err := u.fetchChecksum(ctx, checksumAsset.BrowserDownloadURL, binaryAsset.Name)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "Updating %s from %s to %s\n", appName, currentVersionText, latestVersion)
	if err := u.downloadAndReplace(ctx, exePath, binaryAsset.BrowserDownloadURL, expectedChecksum); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Updated %s to %s\n", appName, latestVersion)
	return nil
}

func (u *githubReleaseUpdater) fetchLatestRelease(ctx context.Context) (githubRelease, error) {
	resp, err := u.get(ctx, u.latestReleaseURL)
	if err != nil {
		return githubRelease{}, fmt.Errorf("fetch latest release metadata: %w", err)
	}
	defer resp.Body.Close()

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return githubRelease{}, fmt.Errorf("decode latest release metadata: %w", err)
	}
	if release.TagName == "" {
		return githubRelease{}, fmt.Errorf("latest release metadata did not include a tag name")
	}
	return release, nil
}

func (u *githubReleaseUpdater) fetchChecksum(ctx context.Context, checksumURL, binaryAssetName string) (string, error) {
	resp, err := u.get(ctx, checksumURL)
	if err != nil {
		return "", fmt.Errorf("download checksum asset %q: %w", binaryAssetName+".sha256", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read checksum asset %q: %w", binaryAssetName+".sha256", err)
	}

	checksum, err := parseSHA256File(string(body), binaryAssetName)
	if err != nil {
		return "", fmt.Errorf("parse checksum asset %q: %w", binaryAssetName+".sha256", err)
	}
	return checksum, nil
}

func (u *githubReleaseUpdater) downloadAndReplace(ctx context.Context, exePath, assetURL, expectedChecksum string) error {
	if strings.EqualFold(u.goos, "windows") {
		return fmt.Errorf("selfupdate is not supported on windows yet")
	}

	info, err := os.Stat(exePath)
	if err != nil {
		return fmt.Errorf("stat current executable %q: %w", exePath, err)
	}

	dir := filepath.Dir(exePath)
	tempFile, err := os.CreateTemp(dir, "."+appName+"-selfupdate-*")
	if err != nil {
		return fmt.Errorf("create temporary download file: %w", err)
	}

	tempPath := tempFile.Name()
	tempClosed := false
	removeTemp := true
	defer func() {
		if !tempClosed {
			_ = tempFile.Close()
		}
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()

	resp, err := u.get(ctx, assetURL)
	if err != nil {
		return fmt.Errorf("download release asset: %w", err)
	}
	defer resp.Body.Close()

	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tempFile, hash), resp.Body); err != nil {
		return fmt.Errorf("write downloaded release asset: %w", err)
	}
	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("flush downloaded release asset: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close downloaded release asset: %w", err)
	}
	tempClosed = true

	actualChecksum := hex.EncodeToString(hash.Sum(nil))
	if !strings.EqualFold(actualChecksum, expectedChecksum) {
		return fmt.Errorf("checksum mismatch for downloaded binary: got %s want %s", actualChecksum, expectedChecksum)
	}

	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	if err := os.Chmod(tempPath, mode); err != nil {
		return fmt.Errorf("mark downloaded binary executable: %w", err)
	}

	if err := os.Rename(tempPath, exePath); err != nil {
		return fmt.Errorf("replace current executable %q: %w", exePath, err)
	}
	removeTemp = false
	return nil
}

func (u *githubReleaseUpdater) get(ctx context.Context, url string) (*http.Response, error) {
	client := u.client
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", appName+"/"+appVersion)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = resp.Status
		}
		return nil, fmt.Errorf("GET %s returned %s: %s", url, resp.Status, message)
	}
	return resp, nil
}

func (r githubRelease) assetsForRuntime(goos, goarch string) (githubReleaseAsset, githubReleaseAsset, error) {
	binaryCandidates, checksumCandidates := releaseAssetCandidates(goos, goarch)
	binaryAsset, ok := findReleaseAsset(r.Assets, binaryCandidates...)
	if !ok {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("latest release does not include a binary for %s/%s", goos, goarch)
	}
	checksumAsset, ok := findReleaseAsset(r.Assets, checksumCandidates...)
	if !ok {
		return githubReleaseAsset{}, githubReleaseAsset{}, fmt.Errorf("latest release does not include a checksum for %s", binaryAsset.Name)
	}
	return binaryAsset, checksumAsset, nil
}

func releaseAssetCandidates(goos, goarch string) ([]string, []string) {
	base := fmt.Sprintf("%s-%s-%s", appName, goos, goarch)
	binaryCandidates := []string{base}
	if goos == "windows" {
		binaryCandidates = append([]string{base + ".exe"}, binaryCandidates...)
	}

	seen := make(map[string]struct{}, len(binaryCandidates))
	checksumCandidates := make([]string, 0, len(binaryCandidates))
	for _, name := range binaryCandidates {
		checksumName := name + ".sha256"
		if _, ok := seen[checksumName]; ok {
			continue
		}
		seen[checksumName] = struct{}{}
		checksumCandidates = append(checksumCandidates, checksumName)
	}
	return binaryCandidates, checksumCandidates
}

func findReleaseAsset(assets []githubReleaseAsset, names ...string) (githubReleaseAsset, bool) {
	for _, name := range names {
		for _, asset := range assets {
			if asset.Name == name {
				return asset, true
			}
		}
	}
	return githubReleaseAsset{}, false
}

func parseSHA256File(content, assetName string) (string, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	unnamedChecksums := make([]string, 0, 1)

	for scanner.Scan() {
		checksum, namedAsset, ok := parseSHA256Line(scanner.Text())
		if !ok {
			continue
		}
		if namedAsset == "" {
			unnamedChecksums = append(unnamedChecksums, checksum)
			continue
		}
		if namedAsset == assetName {
			return checksum, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(unnamedChecksums) == 1 {
		return unnamedChecksums[0], nil
	}
	if len(unnamedChecksums) > 1 {
		return "", fmt.Errorf("checksum file contained multiple unnamed digests")
	}
	return "", fmt.Errorf("checksum for %s not found", assetName)
}

func parseSHA256Line(line string) (string, string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) > 0 && isHexSHA256(fields[0]) {
		name := ""
		if len(fields) > 1 {
			name = strings.TrimPrefix(fields[len(fields)-1], "*")
		}
		return strings.ToLower(fields[0]), name, true
	}

	if idx := strings.LastIndex(line, "="); idx >= 0 {
		candidate := strings.TrimSpace(line[idx+1:])
		if isHexSHA256(candidate) {
			name := ""
			if open := strings.Index(line[:idx], "("); open >= 0 {
				if close := strings.Index(line[open:idx], ")"); close >= 0 {
					name = line[open+1 : open+close]
				}
			}
			return strings.ToLower(candidate), name, true
		}
	}

	return "", "", false
}

func isHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		case r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func normalizeSemverLoose(version string) (string, bool) {
	normalized, err := normalizeSemver(version)
	if err != nil {
		return "", false
	}
	return normalized, true
}

func normalizeSemver(version string) (string, error) {
	version = strings.TrimSpace(version)
	if version == "" {
		return "", fmt.Errorf("version is empty")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}
	parts := strings.Split(version[1:], ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("version must use vMAJOR.MINOR.PATCH")
	}
	for i, part := range parts {
		if part == "" {
			return "", fmt.Errorf("version must use vMAJOR.MINOR.PATCH")
		}
		for _, r := range part {
			if r < '0' || r > '9' {
				return "", fmt.Errorf("version must use vMAJOR.MINOR.PATCH")
			}
		}
		// Strip leading zeros to produce a canonical form.
		parts[i] = strings.TrimLeft(part, "0")
		if parts[i] == "" {
			parts[i] = "0"
		}
	}
	return "v" + strings.Join(parts, "."), nil
}

func compareSemver(a, b string) int {
	aParts := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bParts := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := range 3 {
		ai, _ := strconv.Atoi(aParts[i])
		bi, _ := strconv.Atoi(bParts[i])
		switch {
		case ai > bi:
			return 1
		case ai < bi:
			return -1
		}
	}
	return 0
}
