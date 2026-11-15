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

type browserFixture struct {
	svc        *BrowserService
	db         *database.DB
	cfg        config.Config
	insp       *fakeInspector
	chromePath string
	bravePath  string
}

func writeExe(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("MZ"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func testBrowserFixture(t *testing.T) *browserFixture {
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
	fx := &browserFixture{db: db, cfg: cfg}
	fx.chromePath = filepath.Join(cfg.Layout.Root, "installed", "chrome.exe")
	fx.bravePath = filepath.Join(cfg.Layout.Root, "installed", "brave.exe")
	writeExe(t, fx.chromePath)
	writeExe(t, fx.bravePath)
	fx.insp = &fakeInspector{
		versions: map[string]browserprobe.VersionInfo{
			fx.chromePath: {ProductName: "Google Chrome", FileVersion: "142.0.7444.162"},
			fx.bravePath:  {ProductName: "Brave Browser", FileVersion: "1.73.91"},
		},
		sigs: map[string]browserprobe.Signature{
			fx.bravePath: {Status: "valid", Publisher: "Brave Software, Inc."},
		},
		kernelVersion: "142.0.7444.162",
	}
	fx.svc = fx.newService()
	return fx
}

// newService builds a service over the same database — a second
// call simulates an application restart.
func (fx *browserFixture) newService() *BrowserService {
	svc := NewBrowserService(fx.db, fx.cfg.Layout, slog.Default())
	svc.inspector = fx.insp
	svc.manager = &kernels.Manager{
		Fetcher:   &fakeFetcher{metadata: []byte(testMetadata), zipBytes: testZipForFixture()},
		Inspector: fx.insp,
	}
	return svc
}

var fixtureZip []byte

func testZipForFixture() []byte { return fixtureZip }

func init() {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range map[string]string{
		"chrome-win64/chrome.exe": "MZ",
		"chrome-win64/icudtl.dat": "data",
	} {
		w, _ := zw.Create(name)
		_, _ = w.Write([]byte(body))
	}
	_ = zw.Close()
	fixtureZip = buf.Bytes()
}

// --- tests -----------------------------------------------------------

func TestNoAutoDetection(t *testing.T) {
	fx := testBrowserFixture(t)
	// Browsers exist on disk, but without an explicit user action
	// nothing is listed: DeBrowser never auto-detects.
	got, err := fx.svc.ListBrowsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ListBrowsers without any user action = %+v, want empty", got)
	}
}

func TestManualAddPersistsAcrossRestart(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually()
	if err != nil {
		t.Fatalf("AddBrowserManually: %v", err)
	}
	if added.Source != "manual" || added.Kind != "chrome" || added.DetectionStatus != "verified" || added.LastVerifiedAt == "" {
		t.Fatalf("added = %+v", added)
	}
	// The browser file must NOT be copied into DeBrowser's folders.
	if strings.Contains(added.ExecutablePath, fx.cfg.Layout.Kernels) || strings.Contains(added.ExecutablePath, fx.cfg.Layout.Data) {
		t.Fatalf("browser path points inside DeBrowser data: %s", added.ExecutablePath)
	}
	// A fresh service over the same DB (simulated restart) still
	// lists the registration.
	restarted := fx.newService()
	got, err := restarted.ListBrowsers()
	if err != nil || len(got) != 1 || got[0].ID != added.ID {
		t.Fatalf("after restart ListBrowsers = %+v, %v", got, err)
	}
	// Adding the same browser again refreshes, not duplicates.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	if _, err := fx.svc.AddBrowserManually(); err != nil {
		t.Fatal(err)
	}
	got, _ = fx.svc.ListBrowsers()
	if len(got) != 1 {
		t.Fatalf("duplicate add produced %d records", len(got))
	}
	// Brave can be added too.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.bravePath, nil }
	brave, err := fx.svc.AddBrowserManually()
	if err != nil || brave.Kind != "brave" {
		t.Fatalf("brave add = %+v, %v", brave, err)
	}
}

func TestManualAddRejections(t *testing.T) {
	fx := testBrowserFixture(t)
	cases := []struct {
		name  string
		setup func() string
	}{
		{"chromium is not accepted manually", func() string {
			p := filepath.Join(fx.cfg.Layout.Root, "other", "chromium.exe")
			writeExe(t, p)
			fx.insp.versions[p] = browserprobe.VersionInfo{ProductName: "Chromium", FileVersion: "142.0"}
			return p
		}},
		{"publisher mismatch", func() string {
			p := filepath.Join(fx.cfg.Layout.Root, "other", "fake-chrome.exe")
			writeExe(t, p)
			fx.insp.versions[p] = browserprobe.VersionInfo{ProductName: "Google Chrome", FileVersion: "1.0"}
			fx.insp.sigs[p] = browserprobe.Signature{Status: "valid", Publisher: "Evil Corp"}
			return p
		}},
		{"unsigned", func() string {
			p := filepath.Join(fx.cfg.Layout.Root, "other", "unsigned.exe")
			writeExe(t, p)
			fx.insp.versions[p] = browserprobe.VersionInfo{ProductName: "Google Chrome", FileVersion: "1.0"}
			fx.insp.sigs[p] = browserprobe.Signature{Status: "unsigned"}
			return p
		}},
		{"unknown product", func() string {
			p := filepath.Join(fx.cfg.Layout.Root, "other", "thing.exe")
			writeExe(t, p)
			fx.insp.versions[p] = browserprobe.VersionInfo{ProductName: "Some Tool", FileVersion: "1.0"}
			return p
		}},
		{"missing file", func() string {
			return filepath.Join(fx.cfg.Layout.Root, "other", "gone.exe")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup()
			fx.svc.ExecutablePicker = func() (string, error) { return path, nil }
			if _, err := fx.svc.AddBrowserManually(); err == nil {
				t.Fatal("add must be rejected")
			}
		})
	}
	// Picker cancelled: no record either.
	fx.svc.ExecutablePicker = func() (string, error) { return "", nil }
	if _, err := fx.svc.AddBrowserManually(); err == nil {
		t.Fatal("cancelled picker must error")
	}
	got, _ := fx.svc.ListBrowsers()
	if len(got) != 0 {
		t.Fatalf("rejected adds left records: %+v", got)
	}
}

func TestVerifyBrowserLifecycle(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually()
	if err != nil {
		t.Fatal(err)
	}
	v, err := fx.svc.VerifyBrowser(added.ID)
	if err != nil || v.DetectionStatus != "verified" {
		t.Fatalf("verify ok case = %+v, %v", v, err)
	}
	// Signature goes bad: re-verification marks the record invalid
	// and the outcome persists.
	fx.insp.sigs[fx.chromePath] = browserprobe.Signature{Status: "invalid"}
	v, err = fx.svc.VerifyBrowser(added.ID)
	if err != nil || v.DetectionStatus != "verification_failed" {
		t.Fatalf("verify bad-sig case = %+v, %v", v, err)
	}
	restarted := fx.newService()
	got, _ := restarted.ListBrowsers()
	if len(got) != 1 || got[0].DetectionStatus != "verification_failed" {
		t.Fatalf("invalid status did not persist: %+v", got)
	}
	// File disappears (user uninstalled/moved it): list shows the
	// path as invalid, and verify records it — without searching
	// for a new location.
	if err := os.Remove(fx.chromePath); err != nil {
		t.Fatal(err)
	}
	got, _ = fx.svc.ListBrowsers()
	if len(got) != 1 || got[0].DetectionStatus != "verification_failed" || got[0].StatusReason == "" {
		t.Fatalf("missing file display = %+v", got)
	}
	v, err = fx.svc.VerifyBrowser(added.ID)
	if err != nil || v.DetectionStatus != "verification_failed" {
		t.Fatalf("verify missing case = %+v, %v", v, err)
	}
}

func TestRemoveRegistrationKeepsBrowserFile(t *testing.T) {
	fx := testBrowserFixture(t)
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	added, err := fx.svc.AddBrowserManually()
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RemoveBrowserRegistration(added.ID); err != nil {
		t.Fatalf("RemoveBrowserRegistration: %v", err)
	}
	// The browser binary itself must be completely untouched.
	if _, err := os.Stat(fx.chromePath); err != nil {
		t.Fatalf("browser file was touched by registration removal: %v", err)
	}
	got, _ := fx.svc.ListBrowsers()
	if len(got) != 0 {
		t.Fatalf("registration should be gone: %+v", got)
	}
	if err := fx.svc.RemoveBrowserRegistration(added.ID); err == nil {
		t.Fatal("removing a missing registration must error")
	}
}

func TestKernelLifecycleAndCrossProtection(t *testing.T) {
	fx := testBrowserFixture(t)
	stable, err := fx.svc.GetOfficialStableVersion()
	if err != nil || stable.Version != "142.0.7444.162" {
		t.Fatalf("stable = %+v, %v", stable, err)
	}
	installed, err := fx.svc.InstallManagedKernel()
	if err != nil {
		t.Fatalf("InstallManagedKernel: %v", err)
	}
	if installed.Status != "ready" || !strings.HasPrefix(installed.InstallPath, "kernels") {
		t.Fatalf("installed = %+v", installed)
	}
	if _, err := os.Stat(filepath.Join(fx.cfg.Layout.Kernels, "142.0.7444.162", "chrome.exe")); err != nil {
		t.Fatalf("kernel chrome.exe missing: %v", err)
	}
	if _, err := fx.svc.InstallManagedKernel(); err == nil {
		t.Fatal("duplicate install must fail")
	}
	// Cross-protection: a kernel is not a manual registration and
	// a manual browser is not a kernel.
	fx.svc.ExecutablePicker = func() (string, error) { return fx.chromePath, nil }
	manual, err := fx.svc.AddBrowserManually()
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.svc.RemoveBrowserRegistration(installed.ID); err == nil {
		t.Fatal("RemoveBrowserRegistration must refuse a kernel ID")
	}
	if _, err := os.Stat(filepath.Join(fx.cfg.Layout.Kernels, "142.0.7444.162", "chrome.exe")); err != nil {
		t.Fatalf("kernel was touched by registration removal: %v", err)
	}
	if err := fx.svc.DeleteManagedKernel(manual.ID); err == nil {
		t.Fatal("DeleteManagedKernel must refuse a manual browser ID")
	}
	if _, err := os.Stat(fx.chromePath); err != nil {
		t.Fatalf("manual browser was touched by kernel deletion: %v", err)
	}
	// Unified list separates the two sources.
	all, err := fx.svc.ListBrowsers()
	if err != nil || len(all) != 2 {
		t.Fatalf("ListBrowsers = %+v, %v", all, err)
	}
	sources := map[string]bool{}
	for _, b := range all {
		sources[b.Source] = true
	}
	if !sources["manual"] || !sources["managed"] {
		t.Fatalf("sources = %v, want manual+managed", sources)
	}
	// Verify + delete the kernel properly.
	v, err := fx.svc.VerifyManagedKernel(installed.ID)
	if err != nil || v.Status != "ready" {
		t.Fatalf("verify kernel = %+v, %v", v, err)
	}
	if err := fx.svc.DeleteManagedKernel(installed.ID); err != nil {
		t.Fatalf("DeleteManagedKernel: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fx.cfg.Layout.Kernels, "142.0.7444.162")); !os.IsNotExist(err) {
		t.Fatal("kernel directory should be removed")
	}
}

func TestVerifyManagedKernelDetectsMissingFiles(t *testing.T) {
	fx := testBrowserFixture(t)
	installed, err := fx.svc.InstallManagedKernel()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(fx.cfg.Layout.Kernels, "142.0.7444.162")); err != nil {
		t.Fatal(err)
	}
	v, err := fx.svc.VerifyManagedKernel(installed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != "missing" {
		t.Fatalf("status = %q, want missing", v.Status)
	}
}
