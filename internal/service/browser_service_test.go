package service

import (
	"archive/zip"
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/browserprobe"
	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/kernels"
)

// --- fakes ---------------------------------------------------------

type fakeSource struct{ cands []browserprobe.Candidate }

func (f fakeSource) Candidates(context.Context) ([]browserprobe.Candidate, error) {
	return f.cands, nil
}

type fakeInspector struct {
	versions      map[string]browserprobe.VersionInfo
	sigs          map[string]browserprobe.Signature
	kernelVersion string
}

func (f *fakeInspector) Exists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}
func (f *fakeInspector) VersionInfo(p string) (browserprobe.VersionInfo, error) {
	if v, ok := f.versions[filepath.Clean(p)]; ok {
		return v, nil
	}
	// Managed-kernel chrome.exe inside the kernels tree.
	return browserprobe.VersionInfo{ProductName: "Chromium", FileVersion: f.kernelVersion}, nil
}
func (f *fakeInspector) Signature(p string) (browserprobe.Signature, error) {
	if s, ok := f.sigs[filepath.Clean(p)]; ok {
		return s, nil
	}
	return browserprobe.Signature{Status: "valid", Publisher: "Google LLC"}, nil
}

type fakeFetcher struct {
	metadata []byte
	zipBytes []byte
}

func (f *fakeFetcher) Get(context.Context, string) ([]byte, error) { return f.metadata, nil }
func (f *fakeFetcher) Download(_ context.Context, _, dest string, _ int64, _ func(int64)) error {
	return os.WriteFile(dest, f.zipBytes, 0o600)
}

const testMetadata = `{"channels":{"Stable":{"version":"142.0.7444.162","downloads":{"chrome":[{"platform":"win64","url":"https://storage.googleapis.com/chrome-for-testing-public/142.0.7444.162/win64/chrome-win64.zip"}]}}}}`

func testZip(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"chrome-win64/chrome.exe": "MZ",
		"chrome-win64/icudtl.dat": "data",
	} {
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

func testBrowserService(t *testing.T) (*BrowserService, config.Config, string) {
	t.Helper()
	cfg, err := config.Load(func(key string) string {
		if key == config.EnvAppRoot {
			return t.TempDir()
		}
		return ""
	})
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if err := cfg.Layout.Ensure(); err != nil {
		t.Fatalf("layout ensure: %v", err)
	}
	db, err := database.Open(cfg.Layout.DatabaseFile)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(t.Context()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	svc := NewBrowserService(db, cfg.Layout, slog.Default())

	// One verified system Chrome on disk.
	chromePath := filepath.Join(cfg.Layout.Root, "syschrome", "chrome.exe")
	if err := os.MkdirAll(filepath.Dir(chromePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(chromePath, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	insp := &fakeInspector{
		versions: map[string]browserprobe.VersionInfo{
			chromePath: {ProductName: "Google Chrome", FileVersion: "142.0.7444.162"},
		},
		sigs:          map[string]browserprobe.Signature{},
		kernelVersion: "142.0.7444.162",
	}
	svc.sources = []browserprobe.CandidateSource{fakeSource{cands: []browserprobe.Candidate{
		{Path: chromePath, Scope: browserprobe.ScopeSystem, KindHint: browserprobe.KindChrome},
	}}}
	svc.inspector = insp
	svc.manager = &kernels.Manager{
		Fetcher:   &fakeFetcher{metadata: []byte(testMetadata), zipBytes: testZip(t)},
		Inspector: insp,
	}
	return svc, cfg, chromePath
}

// --- tests -----------------------------------------------------------

func TestDetectSystemBrowsersPersistsNothing(t *testing.T) {
	svc, _, _ := testBrowserService(t)
	got, err := svc.DetectSystemBrowsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "chrome" || got[0].DetectionStatus != "verified" || got[0].Source != "system" {
		t.Fatalf("detection = %+v", got)
	}
	// System browsers must never be written into the kernels table.
	managed, err := svc.ListManagedKernels()
	if err != nil {
		t.Fatal(err)
	}
	if len(managed) != 0 {
		t.Fatalf("system detection leaked into managed kernels: %+v", managed)
	}
}

func TestAddBrowserManuallyVerifies(t *testing.T) {
	svc, cfg, _ := testBrowserService(t)
	bravePath := filepath.Join(cfg.Layout.Root, "brave", "brave.exe")
	if err := os.MkdirAll(filepath.Dir(bravePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bravePath, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if insp, ok := svc.inspector.(*fakeInspector); ok {
		insp.versions[bravePath] = browserprobe.VersionInfo{ProductName: "Brave Browser", FileVersion: "1.73.91"}
		insp.sigs[bravePath] = browserprobe.Signature{Status: "valid", Publisher: "Brave Software, Inc."}
	}
	svc.ExecutablePicker = func() (string, error) { return bravePath, nil }
	added, err := svc.AddBrowserManually()
	if err != nil {
		t.Fatalf("AddBrowserManually: %v", err)
	}
	if added.Kind != "brave" || added.DetectionStatus != "verified" {
		t.Fatalf("added = %+v", added)
	}
	all, _ := svc.DetectSystemBrowsers()
	if len(all) != 2 {
		t.Fatalf("after manual add, detection = %d records, want 2", len(all))
	}

	// An unverifiable file must be rejected, never registered.
	fakePath := filepath.Join(cfg.Layout.Root, "fake", "chrome.exe")
	if err := os.MkdirAll(filepath.Dir(fakePath), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fakePath, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
	if insp, ok := svc.inspector.(*fakeInspector); ok {
		insp.versions[fakePath] = browserprobe.VersionInfo{ProductName: "Google Chrome", FileVersion: "1.0"}
		insp.sigs[fakePath] = browserprobe.Signature{Status: "valid", Publisher: "Evil Corp"}
	}
	svc.ExecutablePicker = func() (string, error) { return fakePath, nil }
	if _, err := svc.AddBrowserManually(); err == nil {
		t.Fatal("publisher-mismatched file must be rejected")
	}
	all, _ = svc.DetectSystemBrowsers()
	if len(all) != 2 {
		t.Fatalf("rejected file was registered anyway: %d records", len(all))
	}
}

func TestInstallVerifyDeleteManagedKernel(t *testing.T) {
	svc, cfg, chromePath := testBrowserService(t)

	stable, err := svc.GetOfficialStableVersion()
	if err != nil || stable.Version != "142.0.7444.162" {
		t.Fatalf("stable = %+v, %v", stable, err)
	}
	installed, err := svc.InstallManagedKernel()
	if err != nil {
		t.Fatalf("InstallManagedKernel: %v", err)
	}
	if installed.Status != "ready" || !strings.HasPrefix(installed.InstallPath, "kernels") {
		t.Fatalf("installed = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Kernels, "142.0.7444.162", "chrome.exe")); err != nil {
		t.Fatalf("kernel chrome.exe missing: %v", err)
	}
	// The system Chrome must be untouched by the install.
	if _, err := os.Stat(chromePath); err != nil {
		t.Fatalf("system chrome was touched: %v", err)
	}
	// Duplicate install is refused.
	if _, err := svc.InstallManagedKernel(); err == nil {
		t.Fatal("duplicate install must fail")
	}
	// Unified list shows both, clearly separated by source.
	all, err := svc.ListBrowsers()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListBrowsers = %+v, %v", all, err)
	}
	// Details resolve for both kinds.
	if d, err := svc.GetBrowserDetails(installed.ID); err != nil || d.Source != "managed" {
		t.Fatalf("managed details = %+v, %v", d, err)
	}
	// Verify passes while intact.
	v, err := svc.VerifyManagedKernel(installed.ID)
	if err != nil || v.Status != "ready" {
		t.Fatalf("verify = %+v, %v", v, err)
	}
	// Deleting a system browser through the kernel path is impossible.
	sysList, _ := svc.DetectSystemBrowsers()
	if err := svc.DeleteManagedKernel(sysList[0].ID); err == nil {
		t.Fatal("deleting a system browser via DeleteManagedKernel must fail")
	}
	if _, err := os.Stat(chromePath); err != nil {
		t.Fatalf("system chrome was deleted: %v", err)
	}
	// Delete the managed kernel: files and registration both go.
	if err := svc.DeleteManagedKernel(installed.ID); err != nil {
		t.Fatalf("DeleteManagedKernel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.Layout.Kernels, "142.0.7444.162")); !os.IsNotExist(err) {
		t.Fatal("kernel directory should be removed")
	}
	managed, _ := svc.ListManagedKernels()
	if len(managed) != 0 {
		t.Fatalf("registration should be removed: %+v", managed)
	}
}

func TestVerifyManagedKernelDetectsMissingFiles(t *testing.T) {
	svc, cfg, _ := testBrowserService(t)
	installed, err := svc.InstallManagedKernel()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(cfg.Layout.Kernels, "142.0.7444.162")); err != nil {
		t.Fatal(err)
	}
	v, err := svc.VerifyManagedKernel(installed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "missing" {
		t.Fatalf("status = %q, want missing", v.Status)
	}
}
