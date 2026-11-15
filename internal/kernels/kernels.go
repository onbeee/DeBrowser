// Package kernels manages DeBrowser's own standalone Chromium
// builds (Stage 9A): Chrome for Testing downloads that DeBrowser
// fetches from Google's official metadata, verifies and installs
// under the portable kernels directory.
//
// Safety posture:
//   - The metadata endpoint is a fixed constant and downloads are
//     accepted only over HTTPS from Google's official hosts; every
//     redirect hop is re-validated. There is no user-supplied URL
//     anywhere in this package.
//   - The official metadata publishes no SHA-256 for these builds,
//     so integrity rests on HTTPS + host whitelist + ZIP structure
//     checks + version match + Windows Authenticode verification
//     of chrome.exe (publisher must be Google LLC). Nothing here
//     claims a hash check that does not exist.
//   - Extraction is bounded (file count, per-file and total size)
//     and path-safe (no absolute paths, no "..", no symlinks), and
//     installation is atomic: a build appears under
//     kernels/<version> only after every check has passed.
//   - Deletion only ever touches a directory that resolves strictly
//     inside the kernels root and is registered as managed.
//
// Network access goes through the Fetcher interface and file
// inspection through browserprobe.Inspector, so every behaviour is
// unit-testable without network or Windows.
package kernels

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/browserprobe"
)

// MetadataURL is the official Chrome for Testing metadata endpoint.
const MetadataURL = "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json"

// Platform is the only supported download platform in this stage.
const Platform = "win64"

// Download and extraction limits (Chrome for Testing win64 builds
// are ~150 MB compressed / ~350 MB extracted; the caps leave ample
// headroom while stopping zip bombs cold).
const (
	maxDownloadBytes = 600 << 20 // 600 MiB
	maxZipFiles      = 2048
	maxFileBytes     = 2 << 30 // 2 GiB per file
	maxTotalBytes    = 4 << 30 // 4 GiB total extracted
	downloadTimeout  = 10 * time.Minute
)

// allowedHosts is the complete set of hosts this package will ever
// talk to.
var allowedHosts = map[string]bool{
	"googlechromelabs.github.io": true,
	"storage.googleapis.com":     true,
}

// ValidateRemoteURL reports whether raw is an HTTPS URL on an
// allowed host. It is applied to the metadata URL, to every
// redirect hop and to the download URL taken from the metadata.
func ValidateRemoteURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("kernels: parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("kernels: url must use https: %q", raw)
	}
	if !allowedHosts[u.Hostname()] {
		return fmt.Errorf("kernels: host %q is not an allowed Google download host", u.Hostname())
	}
	return nil
}

// Fetcher abstracts HTTP access so tests never touch the network.
type Fetcher interface {
	Get(ctx context.Context, url string) ([]byte, error)
	Download(ctx context.Context, url, destPath string, maxBytes int64, onBytes func(n int64)) error
}

// HTTPFetcher is the production Fetcher. Its redirect policy
// re-validates every hop against the same rules as the initial
// URL, so a compromised or unexpected redirect cannot smuggle the
// download to another host or scheme.
type HTTPFetcher struct{ Client *http.Client }

func (f HTTPFetcher) httpClient() *http.Client {
	if f.Client != nil {
		return f.Client
	}
	return &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("kernels: too many redirects")
			}
			return ValidateRemoteURL(req.URL.String())
		},
	}
}

func (f HTTPFetcher) Get(ctx context.Context, raw string) ([]byte, error) {
	if err := ValidateRemoteURL(raw); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("kernels: fetch metadata: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kernels: fetch metadata: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("kernels: read metadata: %w", err)
	}
	return body, nil
}

func (f HTTPFetcher) Download(ctx context.Context, raw, destPath string, maxBytes int64, onBytes func(n int64)) error {
	if err := ValidateRemoteURL(raw); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return err
	}
	resp, err := f.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("kernels: download: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("kernels: download: HTTP %d", resp.StatusCode)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("kernels: create temp download: %w", err)
	}
	defer func() { _ = out.Close() }()
	var written int64
	buf := make([]byte, 256<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxBytes {
				return fmt.Errorf("kernels: download exceeds %d bytes", maxBytes)
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return fmt.Errorf("kernels: write download: %w", werr)
			}
			if onBytes != nil {
				onBytes(written)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("kernels: read download: %w", rerr)
		}
	}
	return nil
}

// Release is one downloadable Chrome for Testing build.
type Release struct {
	Version     string
	DownloadURL string
}

// metadata mirrors the official last-known-good-versions JSON.
type metadata struct {
	Channels map[string]struct {
		Version   string `json:"version"`
		Downloads map[string][]struct {
			Platform string `json:"platform"`
			URL      string `json:"url"`
		} `json:"downloads"`
	} `json:"channels"`
}

// ParseStableRelease extracts the Stable win64 Chrome download
// from official metadata and validates its URL.
func ParseStableRelease(body []byte) (Release, error) {
	var md metadata
	if err := json.Unmarshal(body, &md); err != nil {
		return Release{}, fmt.Errorf("kernels: parse metadata: %w", err)
	}
	stable, ok := md.Channels["Stable"]
	if !ok || stable.Version == "" {
		return Release{}, fmt.Errorf("kernels: metadata has no Stable channel")
	}
	for _, dl := range stable.Downloads["chrome"] {
		if dl.Platform == Platform {
			if err := ValidateRemoteURL(dl.URL); err != nil {
				return Release{}, err
			}
			return Release{Version: stable.Version, DownloadURL: dl.URL}, nil
		}
	}
	return Release{}, fmt.Errorf("kernels: metadata has no %s chrome download for Stable", Platform)
}

// ProgressFunc receives install progress: a phase name
// ("downloading", "extracting", "verifying", "installing") and a
// completion percentage, or -1 when indeterminate.
type ProgressFunc func(phase string, percent int)

// Manager installs and removes managed kernels under one root.
type Manager struct {
	Fetcher   Fetcher
	Inspector browserprobe.Inspector
}

var ErrAlreadyInstalled = fmt.Errorf("kernels: this version is already installed")

// Install downloads, verifies and atomically installs the release
// into kernelsRoot/<version>. Any failure removes all temporary
// state and leaves kernelsRoot untouched.
func (m *Manager) Install(ctx context.Context, kernelsRoot string, rel Release, progress ProgressFunc) (installDir string, err error) {
	if progress == nil {
		progress = func(string, int) {}
	}
	if err := ValidateRemoteURL(rel.DownloadURL); err != nil {
		return "", err
	}
	if rel.Version == "" || strings.Contains(rel.Version, "/") || strings.Contains(rel.Version, `\`) || strings.Contains(rel.Version, "..") {
		return "", fmt.Errorf("kernels: invalid version %q", rel.Version)
	}
	target := filepath.Join(kernelsRoot, rel.Version)
	if _, err := os.Lstat(target); err == nil {
		return "", ErrAlreadyInstalled
	}
	if err := os.MkdirAll(kernelsRoot, 0o750); err != nil {
		return "", fmt.Errorf("kernels: create kernels directory: %w", err)
	}
	tmpName := ".tmp-" + randomSuffix()
	tmpDir := filepath.Join(kernelsRoot, tmpName)
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return "", fmt.Errorf("kernels: create temp directory: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	zipPath := filepath.Join(tmpDir, "download.zip")
	progress("downloading", -1)
	if err = m.Fetcher.Download(ctx, rel.DownloadURL, zipPath, maxDownloadBytes, nil); err != nil {
		return "", err
	}
	extractDir := filepath.Join(tmpDir, "extracted")
	progress("extracting", -1)
	if err = extractZip(zipPath, extractDir); err != nil {
		return "", err
	}
	exePath := filepath.Join(extractDir, "chrome.exe")
	progress("verifying", -1)
	if err = m.verifyBuild(exePath, rel.Version); err != nil {
		return "", err
	}
	progress("installing", -1)
	if err = os.Rename(extractDir, target); err != nil {
		return "", fmt.Errorf("kernels: install build: %w", err)
	}
	_ = os.RemoveAll(tmpDir)
	return target, nil
}

// verifyBuild checks an extracted build: chrome.exe present, its
// file version equals the metadata version, and its Authenticode
// signature is valid and issued to Google LLC.
func (m *Manager) verifyBuild(exePath, wantVersion string) error {
	if st, err := os.Stat(exePath); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("kernels: chrome.exe is missing from the downloaded build")
	}
	vi, err := m.Inspector.VersionInfo(exePath)
	if err != nil {
		return fmt.Errorf("kernels: read chrome.exe version: %w", err)
	}
	if vi.FileVersion != wantVersion {
		return fmt.Errorf("kernels: chrome.exe version %q does not match metadata version %q", vi.FileVersion, wantVersion)
	}
	sig, err := m.Inspector.Signature(exePath)
	if err != nil || sig.Status != "valid" || !strings.EqualFold(strings.TrimSpace(sig.Publisher), browserprobe.ExpectedPublisher(browserprobe.KindChromium)) {
		return fmt.Errorf("kernels: chrome.exe signature verification failed (status %q, publisher %q)", sig.Status, sig.Publisher)
	}
	return nil
}

// VerifyInstalled re-checks an installed build in place and
// returns its verified version.
func (m *Manager) VerifyInstalled(installDir, wantVersion string) error {
	return m.verifyBuild(filepath.Join(installDir, "chrome.exe"), wantVersion)
}

// extractZip extracts a Chrome for Testing zip into destDir. The
// archive must consist of a single top-level folder (chrome-win64)
// whose contents land directly in destDir. Absolute paths, ".."
// components, symlinks and limit violations are rejected.
func extractZip(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("kernels: open download as zip: %w", err)
	}
	defer func() { _ = r.Close() }()
	if len(r.File) > maxZipFiles {
		return fmt.Errorf("kernels: archive has too many files (%d)", len(r.File))
	}
	if err := os.MkdirAll(destDir, 0o750); err != nil {
		return fmt.Errorf("kernels: create extract directory: %w", err)
	}
	var total uint64
	for _, f := range r.File {
		name := filepath.ToSlash(f.Name)
		parts := strings.Split(name, "/")
		if len(parts) < 2 || parts[0] != "chrome-win64" || filepath.IsAbs(name) {
			return fmt.Errorf("kernels: unexpected archive entry %q", f.Name)
		}
		rel := filepath.Join(parts[1:]...)
		if rel == "." {
			continue // the top-level folder entry itself
		}
		if strings.Contains(rel, "..") || filepath.IsAbs(rel) {
			return fmt.Errorf("kernels: archive entry escapes target: %q", f.Name)
		}
		mode := f.Mode()
		if mode&os.ModeSymlink != 0 {
			return fmt.Errorf("kernels: archive entry is a symlink: %q", f.Name)
		}
		dest := filepath.Join(destDir, rel)
		if !strings.HasPrefix(dest, destDir+string(os.PathSeparator)) {
			return fmt.Errorf("kernels: archive entry escapes target: %q", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(dest, 0o750); err != nil {
				return fmt.Errorf("kernels: create directory: %w", err)
			}
			continue
		}
		if f.UncompressedSize64 > maxFileBytes {
			return fmt.Errorf("kernels: archive file too large: %q", f.Name)
		}
		total += f.UncompressedSize64
		if total > maxTotalBytes {
			return fmt.Errorf("kernels: archive extracts to more than %d bytes", maxTotalBytes)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
			return fmt.Errorf("kernels: create directory: %w", err)
		}
		rc, err := f.Open()
		if err != nil {
			return fmt.Errorf("kernels: open archive entry %q: %w", f.Name, err)
		}
		w, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
		if err != nil {
			_ = rc.Close()
			return fmt.Errorf("kernels: create file %q: %w", f.Name, err)
		}
		_, copyErr := io.Copy(w, io.LimitReader(rc, maxFileBytes+1))
		closeErr := w.Close()
		_ = rc.Close()
		if copyErr != nil {
			return fmt.Errorf("kernels: extract %q: %w", f.Name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("kernels: extract %q: %w", f.Name, closeErr)
		}
	}
	return nil
}

// ResolveManagedDir resolves a stored relative install path
// against the kernels root and proves the result is a real
// directory strictly inside that root: never the root itself,
// never outside it, never through a symlink at the final level.
func ResolveManagedDir(kernelsRoot, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("kernels: install path %q is not a relative path", rel)
	}
	rootAbs, err := filepath.EvalSymlinks(kernelsRoot)
	if err != nil {
		return "", fmt.Errorf("kernels: resolve kernels directory: %w", err)
	}
	joined := filepath.Join(rootAbs, rel)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("kernels: resolve install path: %w", err)
	}
	if resolved == rootAbs || !strings.HasPrefix(resolved, rootAbs+string(os.PathSeparator)) {
		return "", fmt.Errorf("kernels: install path %q escapes the kernels directory", rel)
	}
	st, err := os.Lstat(joined)
	if err != nil {
		return "", fmt.Errorf("kernels: stat install path: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return "", fmt.Errorf("kernels: install path %q is not a real directory", rel)
	}
	return resolved, nil
}

// RemoveManaged deletes a managed kernel directory after the full
// ResolveManagedDir verification.
func RemoveManaged(kernelsRoot, rel string) error {
	dir, err := ResolveManagedDir(kernelsRoot, rel)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("kernels: remove kernel directory: %w", err)
	}
	return nil
}

func randomSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "fallback"
	}
	return hex.EncodeToString(b[:])
}
