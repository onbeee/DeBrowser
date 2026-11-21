package mihomo

import (
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CoreVersion is the pinned official Mihomo release DeBrowser
// runs (the version the Stage 6C PoC verified).
const CoreVersion = "v1.19.34"

// releaseAsset is one official release artifact: its file name on
// the GitHub release page, the SHA-256 of the artifact itself and
// the SHA-256 of the executable inside it. Both are verified at
// download/install time; the Windows executable hash is the value
// recorded and cross-checked during the Stage 6C PoC.
type releaseAsset struct {
	file      string
	sha256    string
	exeSHA256 string
}

// Release assets of the pinned version, per platform. The Windows
// hash is the one recorded during the PoC (the downloaded zip);
// the Linux asset exists so development and tests on Linux run
// the same core version.
var releaseAssets = map[string]releaseAsset{
	"windows/amd64": {
		file:      "mihomo-windows-amd64-" + CoreVersion + ".zip",
		sha256:    "f6b855de8722781caea62686f0301f89625fc558d48c1103ffdd98dad7f7f488",
		exeSHA256: "3252ed9449c0e904fd07452b18366b90da77e14a5696e567de11d7839d6cf4b6",
	},
	"linux/amd64": {
		file:      "mihomo-linux-amd64-" + CoreVersion + ".gz",
		sha256:    "223fab877e5795ce780ba39902e5f0b1113c1256ae244a7160f1165e017e9ccc",
		exeSHA256: "0a68807c7c93963bb66d07717fe0f3ac3945b422b525f8a1e19438d278d7ae11",
	},
}

// ReleaseURL returns the official download URL of the pinned
// asset for a GOOS/GOARCH pair.
func ReleaseURL(goos, goarch string) (string, bool) {
	a, ok := releaseAssets[goos+"/"+goarch]
	if !ok {
		return "", false
	}
	return "https://github.com/MetaCubeX/mihomo/releases/download/" + CoreVersion + "/" + a.file, true
}

// binaryName is the core executable's file name on this platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

// BinaryPath returns where the core executable lives inside the
// working directory.
func BinaryPath(workdir string) string {
	return filepath.Join(workdir, binaryName())
}

// EnsureBinary makes sure the pinned core binary exists in the
// working directory, downloading it from the official release and
// verifying the artifact hash when it does not. An existing binary
// is used as-is (the user may also place the official binary there
// manually). Errors name the step that failed; no partial binary
// is ever left behind.
func EnsureBinary(ctx context.Context, workdir string) (string, error) {
	bin := BinaryPath(workdir)
	if st, err := os.Stat(bin); err == nil && st.Size() > 0 {
		return bin, nil
	}
	asset, ok := releaseAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("mihomo: no pinned core asset for %s/%s; place the official %s binary at %s manually",
			runtime.GOOS, runtime.GOARCH, CoreVersion, bin)
	}
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return "", fmt.Errorf("mihomo: create workdir: %w", err)
	}
	url, _ := ReleaseURL(runtime.GOOS, runtime.GOARCH)
	tmp, err := os.CreateTemp(workdir, "download-*")
	if err != nil {
		return "", fmt.Errorf("mihomo: create download temp: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		tmp.Close()
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("mihomo: download core: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		tmp.Close()
		return "", fmt.Errorf("mihomo: download core: status %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 512<<20)); err != nil {
		tmp.Close()
		return "", fmt.Errorf("mihomo: download core: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("mihomo: download core: %w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != asset.sha256 {
		return "", fmt.Errorf("mihomo: downloaded core hash mismatch (got %s)", got)
	}
	if err := extractBinary(tmpName, asset.file, bin); err != nil {
		return "", err
	}
	// Second, independent check: the installed executable itself
	// must hash to the recorded value.
	f, err := os.Open(bin)
	if err != nil {
		return "", fmt.Errorf("mihomo: verify core: %w", err)
	}
	bh := sha256.New()
	_, copyErr := io.Copy(bh, f)
	f.Close()
	if copyErr != nil {
		return "", fmt.Errorf("mihomo: verify core: %w", copyErr)
	}
	if got := hex.EncodeToString(bh.Sum(nil)); got != asset.exeSHA256 {
		os.Remove(bin)
		return "", fmt.Errorf("mihomo: installed core executable hash mismatch (got %s)", got)
	}
	return bin, nil
}

// extractBinary pulls the core executable out of a downloaded
// release artifact (zip on Windows, gzip on Linux) and installs
// it at bin, executable and atomically renamed into place.
func extractBinary(artifactPath, artifactName, bin string) error {
	var data []byte
	var err error
	switch {
	case strings.HasSuffix(artifactName, ".zip"):
		data, err = readZipEntry(artifactPath)
	case strings.HasSuffix(artifactName, ".gz"):
		var f *os.File
		f, err = os.Open(artifactPath)
		if err == nil {
			defer f.Close()
			var gz *gzip.Reader
			gz, err = gzip.NewReader(f)
			if err == nil {
				data, err = io.ReadAll(io.LimitReader(gz, 512<<20))
			}
		}
	default:
		err = fmt.Errorf("unknown artifact format %q", artifactName)
	}
	if err != nil {
		return fmt.Errorf("mihomo: extract core: %w", err)
	}
	tmp := bin + ".tmp"
	if err := os.WriteFile(tmp, data, 0o750); err != nil {
		return fmt.Errorf("mihomo: install core: %w", err)
	}
	if err := os.Rename(tmp, bin); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("mihomo: install core: %w", err)
	}
	return nil
}

// readZipEntry returns the (single) executable entry of a Mihomo
// release zip.
func readZipEntry(path string) ([]byte, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, ".exe") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(io.LimitReader(rc, 512<<20))
		}
	}
	return nil, fmt.Errorf("no .exe entry in release archive")
}

// Manager owns at most one core process: the one this DeBrowser
// run started. It is intentionally small — start, stop, health —
// with no watchdog or restart loop. A core that exits on its own
// is reported through Running()/WaitErr and started again only by
// an explicit Start (UI action or a configuration change).
type Manager struct {
	workdir string
	cfgPath string
	client  *Client

	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// NewManager builds a manager for a working directory and the
// generated configuration file inside it.
func NewManager(workdir, cfgPath string, client *Client) *Manager {
	return &Manager{workdir: workdir, cfgPath: cfgPath, client: client}
}

// Client returns the API client bound to the controller.
func (m *Manager) Client() *Client { return m.client }

// Running reports whether the core process this manager started
// is still alive.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd == nil {
		return false
	}
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

// WaitErr returns the exit error of a finished core process.
func (m *Manager) WaitErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

// Start launches the core with the configuration file and waits
// until its controller answers (or the context/timeout expires).
// A controller that already answers with our credentials (a core
// left running by a previous DeBrowser run) is adopted as healthy
// instead of spawning a duplicate.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.cmd != nil {
		select {
		case <-m.done:
			m.cmd = nil
		default:
			m.mu.Unlock()
			return nil // already running
		}
	}
	m.mu.Unlock()

	if _, err := m.client.Version(ctx); err == nil {
		return nil // a core with our secret already serves the controller
	}
	bin, err := EnsureBinary(ctx, m.workdir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(m.cfgPath); err != nil {
		return fmt.Errorf("mihomo: configuration file missing: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(m.workdir, "core.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("mihomo: open core log: %w", err)
	}
	cmd := exec.Command(bin, "-d", m.workdir, "-f", m.cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	setProcAttrs(cmd)
	if err := cmd.Start(); err != nil {
		logFile.Close()
		return fmt.Errorf("mihomo: start core: %w", err)
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.cmd = cmd
	m.done = done
	m.err = nil
	m.mu.Unlock()
	go func() {
		err := cmd.Wait()
		logFile.Close()
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		close(done)
	}()

	// Readiness: poll the controller until it answers or ~15s pass.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := m.client.Version(ctx); err == nil {
			return nil
		}
		select {
		case <-done:
			return fmt.Errorf("mihomo: core exited during startup (%v); see core.log", m.WaitErr())
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mihomo: core did not become ready in time; see core.log")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Stop terminates the core process this manager started. It never
// touches a core it did not start (for example the user's Clash).
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd, done := m.cmd, m.done
	m.mu.Unlock()
	if cmd == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	m.mu.Lock()
	m.cmd = nil
	m.mu.Unlock()
	return nil
}

// Validate runs the core binary's own configuration check
// (mihomo -t). It is the same validation the core performs at
// load, so a configuration that passes here loads there.
func Validate(ctx context.Context, workdir, cfgPath string) error {
	bin, err := EnsureBinary(ctx, workdir)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, "-t", "-d", workdir, "-f", cfgPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mihomo: configuration check failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ProbePort reports whether something accepts TCP connections on
// 127.0.0.1:port right now. A core that is alive with one listener
// down (port conflict) stays alive, so listener health must be
// probed per port, never inferred from process liveness.
func ProbePort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
