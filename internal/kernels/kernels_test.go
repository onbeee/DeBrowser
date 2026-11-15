package kernels

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/browserprobe"
)

type fakeFetcher struct {
	metadata []byte
	zipBytes []byte
	err      error
}

func (f *fakeFetcher) Get(context.Context, string) ([]byte, error) { return f.metadata, f.err }
func (f *fakeFetcher) Download(_ context.Context, _, dest string, _ int64, _ func(int64)) error {
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(dest, f.zipBytes, 0o600)
}

type fakeInspector struct {
	version   string
	signature browserprobe.Signature
}

func (f *fakeInspector) Exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
func (f *fakeInspector) VersionInfo(string) (browserprobe.VersionInfo, error) {
	return browserprobe.VersionInfo{ProductName: "Chromium", FileVersion: f.version}, nil
}
func (f *fakeInspector) Signature(string) (browserprobe.Signature, error) { return f.signature, nil }

func goodInspector() *fakeInspector {
	return &fakeInspector{version: "142.0.7444.162", signature: browserprobe.Signature{Status: "valid", Publisher: "Google LLC"}}
}

func makeZip(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range entries {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func goodZip(t *testing.T) []byte {
	return makeZip(t, map[string]string{
		"chrome-win64/chrome.exe":     "MZ fake exe",
		"chrome-win64/icudtl.dat":     "data",
		"chrome-win64/locales/en.pak": "pak",
	})
}

const goodMetadata = `{"channels":{"Stable":{"version":"142.0.7444.162","downloads":{"chrome":[{"platform":"win64","url":"https://storage.googleapis.com/chrome-for-testing-public/142.0.7444.162/win64/chrome-win64.zip"}]}}}}`

func TestParseStableRelease(t *testing.T) {
	rel, err := ParseStableRelease([]byte(goodMetadata))
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "142.0.7444.162" || !strings.HasPrefix(rel.DownloadURL, "https://storage.googleapis.com/") {
		t.Fatalf("release = %+v", rel)
	}
}

func TestParseStableReleaseRejectsBadSources(t *testing.T) {
	cases := map[string]string{
		"http scheme":    `{"channels":{"Stable":{"version":"1","downloads":{"chrome":[{"platform":"win64","url":"http://storage.googleapis.com/x.zip"}]}}}}`,
		"evil host":      `{"channels":{"Stable":{"version":"1","downloads":{"chrome":[{"platform":"win64","url":"https://evil.example.com/x.zip"}]}}}}`,
		"lookalike host": `{"channels":{"Stable":{"version":"1","downloads":{"chrome":[{"platform":"win64","url":"https://storage.googleapis.com.evil.com/x.zip"}]}}}}`,
		"no win64":       `{"channels":{"Stable":{"version":"1","downloads":{"chrome":[{"platform":"linux64","url":"https://storage.googleapis.com/x.zip"}]}}}}`,
		"garbage":        `not json`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStableRelease([]byte(body)); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestValidateRemoteURL(t *testing.T) {
	for _, u := range []string{
		"https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json",
		"https://storage.googleapis.com/chrome-for-testing-public/x.zip",
	} {
		if err := ValidateRemoteURL(u); err != nil {
			t.Fatalf("%s should be allowed: %v", u, err)
		}
	}
	for _, u := range []string{"http://storage.googleapis.com/x", "https://example.com/x", "ftp://storage.googleapis.com/x", "://bad"} {
		if err := ValidateRemoteURL(u); err == nil {
			t.Fatalf("%s should be rejected", u)
		}
	}
}

func TestInstallHappyPath(t *testing.T) {
	root := t.TempDir()
	m := &Manager{Fetcher: &fakeFetcher{zipBytes: goodZip(t)}, Inspector: goodInspector()}
	dir, err := m.Install(context.Background(), root, Release{Version: "142.0.7444.162", DownloadURL: "https://storage.googleapis.com/x.zip"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "142.0.7444.162") {
		t.Fatalf("dir = %s", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "chrome.exe")); err != nil {
		t.Fatalf("chrome.exe missing after install: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "locales", "en.pak")); err != nil {
		t.Fatalf("nested file missing: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("kernels root should contain exactly the version dir, got %d entries", len(entries))
	}
}

func TestInstallRejections(t *testing.T) {
	slipZip := makeZip(t, map[string]string{
		"chrome-win64/chrome.exe":  "MZ",
		"chrome-win64/../evil.exe": "evil",
	})
	noExeZip := makeZip(t, map[string]string{"chrome-win64/readme.txt": "hi"})
	corrupt := []byte("this is not a zip at all")
	cases := []struct {
		name      string
		zipBytes  []byte
		inspector *fakeInspector
		fetchErr  error
	}{
		{"zip slip", slipZip, goodInspector(), nil},
		{"missing chrome.exe", noExeZip, goodInspector(), nil},
		{"corrupt zip", corrupt, goodInspector(), nil},
		{"version mismatch", goodZip(t), &fakeInspector{version: "1.0.0.0", signature: browserprobe.Signature{Status: "valid", Publisher: "Google LLC"}}, nil},
		{"signature invalid", goodZip(t), &fakeInspector{version: "142.0.7444.162", signature: browserprobe.Signature{Status: "invalid"}}, nil},
		{"signature wrong publisher", goodZip(t), &fakeInspector{version: "142.0.7444.162", signature: browserprobe.Signature{Status: "valid", Publisher: "Evil Corp"}}, nil},
		{"download cancelled", nil, goodInspector(), context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			m := &Manager{Fetcher: &fakeFetcher{zipBytes: tc.zipBytes, err: tc.fetchErr}, Inspector: tc.inspector}
			_, err := m.Install(context.Background(), root, Release{Version: "142.0.7444.162", DownloadURL: "https://storage.googleapis.com/x.zip"}, nil)
			if err == nil {
				t.Fatal("expected install to fail")
			}
			entries, _ := os.ReadDir(root)
			if len(entries) != 0 {
				t.Fatalf("failed install left %d entries behind (temp not cleaned)", len(entries))
			}
		})
	}
}

func TestInstallDuplicateRejected(t *testing.T) {
	root := t.TempDir()
	m := &Manager{Fetcher: &fakeFetcher{zipBytes: goodZip(t)}, Inspector: goodInspector()}
	rel := Release{Version: "142.0.7444.162", DownloadURL: "https://storage.googleapis.com/x.zip"}
	if _, err := m.Install(context.Background(), root, rel, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Install(context.Background(), root, rel, nil); !errors.Is(err, ErrAlreadyInstalled) {
		t.Fatalf("second install err = %v, want ErrAlreadyInstalled", err)
	}
}

func TestResolveAndRemoveManaged(t *testing.T) {
	root := t.TempDir()
	ver := filepath.Join(root, "142.0.7444.162")
	if err := os.MkdirAll(ver, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ver, "chrome.exe"), []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	// rel is relative to the kernels root itself. The result is a
	// fully resolved real path, so compare against the resolved
	// form of the expectation (temp dirs may contain alias
	// components, e.g. Windows 8.3 short names).
	want, err := filepath.EvalSymlinks(ver)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ResolveManagedDir(root, "142.0.7444.162")
	if err != nil || got != want {
		t.Fatalf("resolve = %s, %v (want %s)", got, err, want)
	}
	for _, bad := range []string{"", ".", "..", "../outside", "/abs/path"} {
		if _, err := ResolveManagedDir(root, bad); err == nil {
			t.Fatalf("rel %q should be rejected", bad)
		}
	}
	if err := RemoveManaged(root, "142.0.7444.162"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ver); !os.IsNotExist(err) {
		t.Fatal("kernel dir should be gone")
	}
	// Removing the root itself must be impossible.
	if err := RemoveManaged(root, "."); err == nil {
		t.Fatal("removing the kernels root must be refused")
	}
}
