package service

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/mihomocore"
	"github.com/onbeee/DeBrowser/internal/paths"
)

// coreFixture builds a CoreService over a temp layout with a
// dummy installed binary and version seams, plus an httptest
// GitHub API/download server.
type coreFixture struct {
	svc     *CoreService
	mgr     *mihomocore.Manager
	version string // what versionFn reports for any binary
	zipSHA  string
}

func buildCoreZip(t *testing.T, exeContent []byte) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("mihomo-windows-amd64.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(exeContent); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func newCoreFixture(t *testing.T, latest string, current string) *coreFixture {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	mgr := mihomocore.NewManager(layout, nil)
	if err := os.MkdirAll(layout.Mihomo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mgr.BinaryPath(), []byte("installed-core-"+current), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &coreFixture{mgr: mgr, version: current}
	zipBytes, zipSHA := buildCoreZip(t, []byte("installed-core-"+latest))
	f.zipSHA = zipSHA

	var api *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		assetURL := api.URL + "/asset.zip"
		fmt.Fprintf(w, `{"tag_name":%q,"prerelease":false,"assets":[{"name":"mihomo-windows-amd64-%s.zip","browser_download_url":%q,"digest":"sha256:%s"}]}`,
			latest, latest, assetURL, zipSHA)
	})
	mux.HandleFunc("/asset.zip", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(zipBytes)
	})
	api = httptest.NewServer(mux)
	t.Cleanup(api.Close)

	svc := NewCoreService(mgr, nil)
	svc.apiBase = api.URL
	svc.versionFn = func(context.Context, string) (string, error) { return f.version, nil }
	f.svc = svc
	return f
}

func TestCoreCheckUpToDate(t *testing.T) {
	f := newCoreFixture(t, "v1.19.34", "v1.19.34")
	view, err := f.svc.CheckCoreUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "up-to-date" || view.LatestVersion != "v1.19.34" || view.CurrentVersion != "v1.19.34" || view.CheckedAt == "" {
		t.Fatalf("view = %+v", view)
	}
	if _, err := f.svc.UpdateCore(); err == nil {
		t.Fatal("UpdateCore accepted with no update available")
	}
}

func TestCoreCheckAndUpdate(t *testing.T) {
	f := newCoreFixture(t, "v1.19.35", "v1.19.34")
	view, err := f.svc.CheckCoreUpdate()
	if err != nil {
		t.Fatal(err)
	}
	if view.Status != "update-available" {
		t.Fatalf("status = %q, want update-available", view.Status)
	}
	// The "new binary" reports the new version once installed:
	// versionFn must reflect the swap. Simulate by making the
	// seam answer per-path content.
	f.svc.versionFn = func(_ context.Context, path string) (string, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		if strings.Contains(string(data), "v1.19.35") {
			return "v1.19.35", nil
		}
		return "v1.19.34", nil
	}
	done, err := f.svc.UpdateCore()
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != "up-to-date" || done.CurrentVersion != "v1.19.35" {
		t.Fatalf("after update view = %+v", done)
	}
	data, _ := os.ReadFile(f.mgr.BinaryPath())
	if string(data) != "installed-core-v1.19.35" {
		t.Fatalf("installed binary = %q", data)
	}
	bak, err := os.ReadFile(f.mgr.BackupPath())
	if err != nil || string(bak) != "installed-core-v1.19.34" {
		t.Fatalf("rollback backup = %q, %v", bak, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(f.mgr.BinaryPath()), ".update-download.zip")); !os.IsNotExist(err) {
		t.Fatal("download temp file left behind")
	}
}

func TestCoreUpdateRejectsSHAMismatch(t *testing.T) {
	f := newCoreFixture(t, "v1.19.35", "v1.19.34")
	// Corrupt the digest the API reports.
	f.svc.apiBase = strings.Replace(f.svc.apiBase, "http://", "http://", 1)
	orig := f.svc.CheckCoreUpdate
	_ = orig
	if _, err := f.svc.CheckCoreUpdate(); err != nil {
		t.Fatal(err)
	}
	f.svc.mu.Lock()
	f.svc.assetSHA256 = strings.Repeat("0", 64)
	f.svc.mu.Unlock()
	f.svc.versionFn = func(context.Context, string) (string, error) { return "v1.19.35", nil }
	if _, err := f.svc.UpdateCore(); err == nil {
		t.Fatal("SHA-mismatched update was installed")
	}
	data, _ := os.ReadFile(f.mgr.BinaryPath())
	if string(data) != "installed-core-v1.19.34" {
		t.Fatalf("binary after rejected update = %q, want the old core intact", data)
	}
	if _, err := os.Stat(f.mgr.BackupPath()); !os.IsNotExist(err) {
		t.Fatal("backup created by a rejected update (swap must never start)")
	}
}

func TestCoreCheckFailureIsNotUpToDate(t *testing.T) {
	f := newCoreFixture(t, "v1.19.35", "v1.19.34")
	f.svc.apiBase = "http://127.0.0.1:1/unreachable"
	view, err := f.svc.CheckCoreUpdate()
	if err == nil {
		t.Fatal("unreachable API reported success")
	}
	if view.Status != "check-failed" {
		t.Fatalf("status = %q, want check-failed", view.Status)
	}
}

func TestCompareVersions(t *testing.T) {
	if compareVersions("v1.19.34", "v1.19.35") >= 0 || compareVersions("v1.20.0", "v1.19.99") <= 0 || compareVersions("v1.19.34", "v1.19.34") != 0 {
		t.Fatal("version comparison wrong")
	}
}
