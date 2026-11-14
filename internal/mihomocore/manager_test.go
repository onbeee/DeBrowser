package mihomocore

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/paths"
)

type fakeProcess struct {
	pid    int
	killed chan struct{}
	once   sync.Once
}

func (p *fakeProcess) Pid() int { return p.pid }
func (p *fakeProcess) Kill() error {
	p.once.Do(func() { close(p.killed) })
	return nil
}
func (p *fakeProcess) Wait() error {
	<-p.killed
	return nil
}

func testLayout(t *testing.T) paths.Layout {
	t.Helper()
	layout, err := paths.Resolve(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := layout.Ensure(); err != nil {
		t.Fatal(err)
	}
	return layout
}

func TestParseVersionOutput(t *testing.T) {
	v, err := ParseVersionOutput("Mihomo Meta v1.19.34 windows amd64 with go1.25.0 2026-11-11T00:00:00Z\n")
	if err != nil || v != "v1.19.34" {
		t.Fatalf("ParseVersionOutput = %q, %v", v, err)
	}
	if _, err := ParseVersionOutput("something else entirely"); err == nil {
		t.Fatal("garbage output accepted")
	}
}

func TestEnsureInstalledCopiesBundled(t *testing.T) {
	layout := testLayout(t)
	m := NewManager(layout, nil)
	exeDir := t.TempDir()
	bundled := filepath.Join(exeDir, "mihomo")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundled, BinaryName()), []byte("fake-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.exeDir = exeDir
	if m.Installed() {
		t.Fatal("installed before EnsureInstalled")
	}
	if err := m.EnsureInstalled(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(m.BinaryPath())
	if err != nil || string(data) != "fake-core" {
		t.Fatalf("installed binary = %q, %v", data, err)
	}
	// A second call never overwrites an existing install.
	if err := os.WriteFile(m.BinaryPath(), []byte("updated-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.EnsureInstalled(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(m.BinaryPath())
	if string(data) != "updated-core" {
		t.Fatal("EnsureInstalled overwrote the installed core")
	}
}

func TestStartStopStatusAndLog(t *testing.T) {
	layout := testLayout(t)
	m := NewManager(layout, nil)
	if err := os.WriteFile(m.BinaryPath(), []byte("fake-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	m.runVersion = func(context.Context, string) (string, error) { return "v1.19.34", nil }
	proc := &fakeProcess{pid: 4321, killed: make(chan struct{})}
	m.launch = func(binary, homeDir, configPath string, stdout io.Writer) (process, error) {
		if _, err := os.Stat(configPath); err != nil {
			return nil, err
		}
		cfg, _ := os.ReadFile(configPath)
		text := string(cfg)
		if !strings.Contains(text, "external-controller: 127.0.0.1:") {
			return nil, errString("config lacks loopback controller")
		}
		if strings.Contains(text, "mixed-port") || strings.Contains(text, "tun:") {
			return nil, errString("minimal config must not open proxy listeners or TUN")
		}
		_, _ = io.WriteString(stdout, "core log line\n")
		return proc, nil
	}
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st := m.Status(ctx)
	if !st.Running || st.PID != 4321 || st.Version != "v1.19.34" || !st.Installed {
		t.Fatalf("status = %+v", st)
	}
	if err := m.Start(ctx); err == nil {
		t.Fatal("second Start accepted while running")
	}
	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !m.Status(ctx).Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if m.Status(ctx).Running {
		t.Fatal("core still running after Stop")
	}
	lines, err := m.TailLog(10)
	if err != nil || len(lines) != 1 || lines[0] != "core log line" {
		t.Fatalf("TailLog = %v, %v", lines, err)
	}
	// Shutdown on a stopped core is a no-op.
	m.Shutdown()
}

type errString string

func (e errString) Error() string { return string(e) }

func TestSwapBinaryRollback(t *testing.T) {
	layout := testLayout(t)
	m := NewManager(layout, nil)
	if err := os.WriteFile(m.BinaryPath(), []byte("old-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(layout.Mihomo, ".update-mihomo.exe")
	if err := os.WriteFile(newPath, []byte("new-core"), 0o755); err != nil {
		t.Fatal(err)
	}
	rollback, err := m.SwapBinary(newPath)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(m.BinaryPath())
	if string(data) != "new-core" {
		t.Fatalf("after swap = %q", data)
	}
	bak, _ := os.ReadFile(m.BackupPath())
	if string(bak) != "old-core" {
		t.Fatalf("backup = %q", bak)
	}
	if err := rollback(); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(m.BinaryPath())
	if string(data) != "old-core" {
		t.Fatalf("after rollback = %q", data)
	}
}
