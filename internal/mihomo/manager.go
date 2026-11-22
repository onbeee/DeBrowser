package mihomo

// Manager owns the ONE Mihomo core process DeBrowser runs: it
// installs the pinned official binary on first use (download +
// SHA-256 verification), starts it against the generated
// configuration, and stops it again. It manages only the
// process it started itself — an occupied controller port is
// reported, never adopted or killed.

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// CoreVersion is the pinned official Mihomo release.
const CoreVersion = "v1.19.34"

type releaseAsset struct {
	file      string
	sha256    string
	exeSHA256 string
}

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

func releaseURL() (string, releaseAsset, bool) {
	a, ok := releaseAssets[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		return "", releaseAsset{}, false
	}
	return "https://github.com/MetaCubeX/mihomo/releases/download/" + CoreVersion + "/" + a.file, a, true
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

// BinaryPath is where the core executable lives inside the
// core's working directory.
func BinaryPath(workdir string) string {
	return filepath.Join(workdir, binaryName())
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EnsureBinary makes sure the pinned core binary is present in
// workdir: an existing file is verified against the pinned
// executable hash (a foreign file is refused, never used); a
// missing one is downloaded from the official release and
// verified twice (artifact hash, then executable hash) before
// it lands.
func EnsureBinary(ctx context.Context, workdir string) error {
	_, asset, ok := releaseURL()
	if !ok {
		return fmt.Errorf("当前平台没有可用的 Mihomo 核心发行包（%s/%s）", runtime.GOOS, runtime.GOARCH)
	}
	bin := BinaryPath(workdir)
	if data, err := os.ReadFile(bin); err == nil {
		if sha256Hex(data) == asset.exeSHA256 {
			return nil
		}
		return fmt.Errorf("核心目录中的可执行文件与官方版本不一致，已拒绝使用：%s", bin)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("读取核心可执行文件失败：%w", err)
	}
	url, _, _ := releaseURL()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("下载 Mihomo 核心失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 Mihomo 核心失败：官方服务器返回 %d", resp.StatusCode)
	}
	artifact, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return fmt.Errorf("下载 Mihomo 核心失败：%w", err)
	}
	if sha256Hex(artifact) != asset.sha256 {
		return fmt.Errorf("下载的 Mihomo 核心校验失败（与官方发布不一致），已放弃安装")
	}
	exe, err := extractExecutable(artifact, asset.file)
	if err != nil {
		return err
	}
	if sha256Hex(exe) != asset.exeSHA256 {
		return fmt.Errorf("下载的 Mihomo 可执行文件校验失败（与官方发布不一致），已放弃安装")
	}
	if err := os.MkdirAll(workdir, 0o750); err != nil {
		return err
	}
	tmp := bin + ".tmp"
	if err := os.WriteFile(tmp, exe, 0o750); err != nil {
		return err
	}
	if err := os.Rename(tmp, bin); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func extractExecutable(artifact []byte, file string) ([]byte, error) {
	if strings.HasSuffix(file, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(artifact), int64(len(artifact)))
		if err != nil {
			return nil, fmt.Errorf("解压 Mihomo 核心失败：%w", err)
		}
		for _, f := range zr.File {
			if strings.HasSuffix(f.Name, ".exe") {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, 256<<20))
			}
		}
		return nil, fmt.Errorf("Mihomo 发行包中没有可执行文件")
	}
	gz, err := gzip.NewReader(bytes.NewReader(artifact))
	if err != nil {
		return nil, fmt.Errorf("解压 Mihomo 核心失败：%w", err)
	}
	defer gz.Close()
	return io.ReadAll(io.LimitReader(gz, 256<<20))
}

// Manager serializes Start/Stop as whole operations: the core
// is either the one child this manager started, or nothing.
type Manager struct {
	workdir string
	opMu    sync.Mutex
	mu      sync.Mutex
	child   childProcess
	done    chan struct{}
	err     error
	logFile *os.File
}

func NewManager(workdir string) *Manager {
	return &Manager{workdir: workdir}
}

// Running reports whether the managed child is alive.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.child == nil {
		return false
	}
	select {
	case <-m.done:
		return false
	default:
		return true
	}
}

// WaitErr returns the managed child's exit error once it has
// exited (nil while running or after a clean exit).
func (m *Manager) WaitErr() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.child == nil {
		return nil
	}
	select {
	case <-m.done:
		return m.err
	default:
		return nil
	}
}

// Start installs the binary if needed and starts the core
// against workdir/config.yaml, waiting until its control API
// answers or the child exits. client must carry the same
// secret the configuration was generated with.
func (m *Manager) Start(ctx context.Context, controllerPort int, client *Client) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	if m.Running() {
		return nil
	}
	m.clearExited()
	if err := EnsureBinary(ctx, m.workdir); err != nil {
		return err
	}
	cfg := filepath.Join(m.workdir, "config.yaml")
	if _, err := os.Stat(cfg); err != nil {
		return fmt.Errorf("核心配置文件不存在：%w", err)
	}
	if ProbePort(controllerPort) {
		return fmt.Errorf("核心控制端口 %d 已被其他程序占用（可能是已在运行的 Clash 或 Mihomo），DeBrowser 不会接管它；请先关闭占用程序后重试", controllerPort)
	}
	logFile, err := os.OpenFile(filepath.Join(m.workdir, "core.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("打开核心日志失败：%w", err)
	}
	child, err := startCoreChild(BinaryPath(m.workdir), []string{"-d", m.workdir, "-f", cfg}, m.workdir, logFile)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("启动 Mihomo 核心失败：%w", err)
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.child = child
	m.done = done
	m.err = nil
	m.logFile = logFile
	m.mu.Unlock()
	go func() {
		err := child.Wait()
		m.mu.Lock()
		m.err = err
		m.mu.Unlock()
		_ = logFile.Close()
		close(done)
	}()
	// Readiness: poll the control API until it answers, the
	// child exits, or the caller's context ends.
	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-done:
			return fmt.Errorf("Mihomo 核心启动后立即退出（%v），详情见核心日志 core.log", m.WaitErr())
		case <-ctx.Done():
			_ = m.stopLocked()
			return ctx.Err()
		default:
		}
		pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := client.Version(pctx)
		cancel()
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			_ = m.stopLocked()
			return fmt.Errorf("Mihomo 核心在 15 秒内没有就绪，详情见核心日志 core.log")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Stop terminates the managed core. It reports honestly: a
// child that cannot be confirmed exited keeps its handle and
// the error says so.
func (m *Manager) Stop() error {
	m.opMu.Lock()
	defer m.opMu.Unlock()
	return m.stopLocked()
}

func (m *Manager) stopLocked() error {
	m.mu.Lock()
	child, done := m.child, m.done
	m.mu.Unlock()
	if child == nil {
		return nil
	}
	select {
	case <-done:
		m.clearExited()
		return nil
	default:
	}
	if err := child.Kill(); err != nil {
		select {
		case <-done:
			m.clearExited()
			return nil
		default:
			return fmt.Errorf("结束 Mihomo 核心失败：%w", err)
		}
	}
	select {
	case <-done:
		m.clearExited()
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("Mihomo 核心在 5 秒内没有退出")
	}
}

func (m *Manager) clearExited() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.child != nil {
		select {
		case <-m.done:
			m.child = nil
			m.done = nil
			m.err = nil
			m.logFile = nil
		default:
		}
	}
}

// ProbePort reports whether something accepts TCP connections
// on 127.0.0.1:port right now.
func ProbePort(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// ProbeListener reports whether a SOCKS5 handshake is answered
// on 127.0.0.1:port (a live core listener). It proves a working
// proxy entry exists at the port, nothing more.
func ProbeListener(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return false
	}
	buf := make([]byte, 2)
	if _, err := io.ReadFull(conn, buf); err != nil {
		return false
	}
	return buf[0] == 0x05 && buf[1] == 0x00
}
