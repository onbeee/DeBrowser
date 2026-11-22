package service

// Tests for the final review fixes (P1-1, P2-2, P2-3): the
// reimport backup read, the binding read in setBinding, and
// DeleteSource's post-commit cleanup logging.

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P1-1: a stored copy that exists but cannot be read aborts
// the reimport BEFORE anything is replaced — the copy is never
// overwritten without a readable backup. (The copy is swapped
// for a directory: reading it fails with a non-NotExist error,
// and a write-over would have destroyed it.)
func TestReimportAbortsWhenOldCopyUnreadable(t *testing.T) {
	f := testProxyFixture(t)
	view := addFileSourceOK(t, f, "文件源", "proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}\n")
	copies, _ := filepath.Glob(filepath.Join(f.svc.sourcesDir(), "*.yaml"))
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want the stored copy", copies)
	}
	copyPath := copies[0]
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(copyPath, 0o750); err != nil {
		t.Fatal(err)
	}
	// A valid new file is picked; the reimport must still abort.
	newFile := filepath.Join(t.TempDir(), "new.yaml")
	writeSourceYAML(t, newFile, "NodeB=18102")
	f.svc.SourceFilePicker = func() (string, error) { return newFile, nil }
	_, err := f.svc.ReimportFileSource("op", view.ID)
	if err == nil {
		t.Fatal("want the reimport to abort on the unreadable stored copy")
	}
	for _, want := range []string{"读取现有文件副本失败", "原文件未被覆盖"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to contain %q", err, want)
		}
	}
	// The stored copy was not overwritten: the directory is
	// still there, and the source is unchanged.
	if info, serr := os.Stat(copyPath); serr != nil || !info.IsDir() {
		t.Fatalf("stored copy: info=%v err=%v, want the directory untouched", info, serr)
	}
	sources, err := f.svc.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ID != view.ID {
		t.Fatalf("sources = %+v, want the original source unchanged", sources)
	}
}

// P2-2: a binding read that fails with anything other than
// "not found" aborts SetBinding — the recorded binding is
// never silently treated as absent (which would reallocate
// the instance's port). The read failure is provoked by
// corrupting the row's port value so the store scan fails.
func TestSetBindingAbortsOnBindingReadError(t *testing.T) {
	f, b := gateHealFixture(t)
	ctx := context.Background()
	if _, err := f.db.SQL().ExecContext(ctx,
		`UPDATE instance_proxy_bindings SET port = 'corrupt' WHERE instance_id = ?`, b.InstanceID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SetBinding("op", b.InstanceID, b.SourceID, "NodeA")
	if err == nil {
		t.Fatal("want SetBinding to abort on the binding read failure")
	}
	if !strings.Contains(err.Error(), "读取实例现有代理绑定失败") {
		t.Fatalf("err = %v, want the binding-read abort", err)
	}
	// The recorded row is untouched: no upsert, no new port.
	var portValue string
	if err := f.db.SQL().QueryRowContext(ctx,
		`SELECT CAST(port AS TEXT) FROM instance_proxy_bindings WHERE instance_id = ?`, b.InstanceID).Scan(&portValue); err != nil {
		t.Fatal(err)
	}
	if portValue != "corrupt" {
		t.Fatalf("recorded port = %q, want the row untouched by the aborted SetBinding", portValue)
	}
}

// P2-3: DeleteSource's post-commit cleanup failures are
// logged, not silent — while the deletion itself still
// succeeds. (The file copy is made un-removable by swapping
// it for a non-empty directory.)
func TestDeleteSourceCleanupFailureIsLogged(t *testing.T) {
	f := testProxyFixture(t)
	var buf bytes.Buffer
	f.svc.log = slog.New(slog.NewTextHandler(&buf, nil))
	view := addFileSourceOK(t, f, "待删源", "proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: 18101}\n")
	copies, _ := filepath.Glob(filepath.Join(f.svc.sourcesDir(), "*.yaml"))
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want the stored copy", copies)
	}
	copyPath := copies[0]
	if err := os.Remove(copyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(copyPath, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(copyPath, "child"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteSource("op", view.ID); err != nil {
		t.Fatalf("the deletion itself must succeed, got %v", err)
	}
	sources, err := f.svc.ListSources()
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("sources = %+v, want the source deleted", sources)
	}
	if !strings.Contains(buf.String(), "file copy cleanup failed") {
		t.Fatalf("log = %q, want the cleanup failure recorded", buf.String())
	}
	// The residue really remains (and is now discoverable via
	// the log line).
	if info, err := os.Stat(copyPath); err != nil || !info.IsDir() {
		t.Fatalf("residue: info=%v err=%v, want the non-empty directory left behind", info, err)
	}
}
