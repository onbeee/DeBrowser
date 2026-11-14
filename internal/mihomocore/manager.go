// Package mihomocore manages the official Mihomo binary's file
// and process lifecycle for DeBrowser: installation from the
// bundled copy, version query, start/stop, status, log tail and
// exit cleanup. The binary itself is the unmodified official
// release (see release.go); nothing here patches, wraps or
// reconfigures the user's system — the core is started with a
// minimal loopback-only configuration, no TUN, no proxy
// listeners, no system proxy changes.
package mihomocore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/paths"
)

// process is the slice of os/exec.Cmd the manager drives; tests
// substitute a fake.
type process interface {
	Pid() int
	Kill() error
	Wait() error
}

type execProcess struct{ cmd *exec.Cmd }

func (p execProcess) Pid() int       { return p.cmd.Process.Pid }
func (p execProcess) Kill() error    { return p.cmd.Process.Kill() }
func (p execProcess) Wait() error    { return p.cmd.Wait() }
func (p execProcess) String() string { return fmt.Sprintf("pid %d", p.Pid()) }

// Status is a snapshot of the core's state.
type Status struct {
	Installed bool
	Version   string
	Running   bool
	PID       int
	StartedAt string
	LastExit  string
}

// Manager owns one Mihomo core installation inside the portable
// data directory:
//
//	data/mihomo/mihomo.exe        the core binary (stable path)
//	data/mihomo/home/config.yaml  the generated minimal config
//	data/logs/mihomo-core.log     the core's own output
//
// The bundled copy ships next to DeBrowser.exe as
// mihomo/mihomo.exe and is copied in on first use, so replacing
// DeBrowser.exe never disturbs the installed core or user data.
type Manager struct {
	layout paths.Layout
	log    *slog.Logger

	// Seams for tests.
	exeDir     string
	launch     func(binary, homeDir, configPath string, stdout io.Writer) (process, error)
	runVersion func(ctx context.Context, binary string) (string, error)

	mu          sync.Mutex
	proc        process
	done        chan struct{}
	startedAt   time.Time
	lastExit    string
	versionMemo string
	logFile     *os.File
}

// NewManager builds a manager over the portable layout.
func NewManager(layout paths.Layout, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	exeDir := ""
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	m := &Manager{layout: layout, log: log, exeDir: exeDir}
	m.launch = m.launchExec
	m.runVersion = runVersionQuery
	return m
}

// BinaryName is the installed core's file name per platform.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "mihomo.exe"
	}
	return "mihomo"
}

// BinaryPath is the stable installed location.
func (m *Manager) BinaryPath() string {
	return filepath.Join(m.layout.Mihomo, BinaryName())
}

// BundledPath is where the shipped copy sits (next to the app
// executable).
func (m *Manager) BundledPath() string {
	return filepath.Join(m.exeDir, "mihomo", BinaryName())
}

// HomeDir is the core's working directory (its config home).
func (m *Manager) HomeDir() string {
	return filepath.Join(m.layout.Mihomo, "home")
}

// LogPath is the core output log inside the portable logs dir.
func (m *Manager) LogPath() string {
	return filepath.Join(m.layout.Logs, "mihomo-core.log")
}

// Installed reports whether the core binary is present.
func (m *Manager) Installed() bool {
	info, err := os.Stat(m.BinaryPath())
	return err == nil && !info.IsDir() && info.Size() > 0
}

// EnsureInstalled copies the bundled core into the portable
// data directory when the installed copy is missing. An
// existing installed core is never overwritten here (updates go
// through the verified update flow in the service layer).
func (m *Manager) EnsureInstalled() error {
	if m.Installed() {
		return nil
	}
	src := m.BundledPath()
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("未找到随附的 Mihomo 核心文件（%s）", src)
	}
	if err := os.MkdirAll(m.layout.Mihomo, 0o755); err != nil {
		return fmt.Errorf("创建核心目录失败：%w", err)
	}
	tmp := m.BinaryPath() + ".tmp"
	if err := copyFile(src, tmp); err != nil {
		return fmt.Errorf("安装 Mihomo 核心失败：%w", err)
	}
	if err := os.Rename(tmp, m.BinaryPath()); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("安装 Mihomo 核心失败：%w", err)
	}
	m.mu.Lock()
	m.versionMemo = ""
	m.mu.Unlock()
	m.log.Info("Mihomo 核心已安装", "path", m.BinaryPath())
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ParseVersionOutput extracts the version tag from the first
// line of `mihomo -v` output, whose format is:
//
//	Mihomo Meta v1.19.34 windows amd64 with go1.25.x <build time>
func ParseVersionOutput(out string) (string, error) {
	line := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	fields := strings.Fields(line)
	if len(fields) >= 3 && fields[0] == "Mihomo" && fields[1] == "Meta" && strings.HasPrefix(fields[2], "v") {
		return fields[2], nil
	}
	return "", fmt.Errorf("无法识别核心版本输出")
}

func runVersionQuery(ctx context.Context, binary string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, binary, "-v").Output()
	if err != nil {
		return "", fmt.Errorf("运行核心版本查询失败：%w", err)
	}
	return ParseVersionOutput(string(out))
}

// Version returns the installed core's actual version by
// running the binary; the result is memoised until the binary
// changes (install/update clears the memo).
func (m *Manager) Version(ctx context.Context) (string, error) {
	m.mu.Lock()
	memo := m.versionMemo
	m.mu.Unlock()
	if memo != "" {
		return memo, nil
	}
	if !m.Installed() {
		return "", fmt.Errorf("Mihomo 核心未安装")
	}
	v, err := m.runVersion(ctx, m.BinaryPath())
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.versionMemo = v
	m.mu.Unlock()
	return v, nil
}

// InvalidateVersionMemo forgets the cached version (used after
// an update replaced the binary).
func (m *Manager) InvalidateVersionMemo() {
	m.mu.Lock()
	m.versionMemo = ""
	m.mu.Unlock()
}

// VersionOf runs the version query against an arbitrary binary
// path (used to verify a freshly downloaded core BEFORE it is
// installed).
func (m *Manager) VersionOf(ctx context.Context, binary string) (string, error) {
	return m.runVersion(ctx, binary)
}

// BackupPath is where the previous binary is kept during an
// update, for rollback.
func (m *Manager) BackupPath() string {
	return m.BinaryPath() + ".bak"
}

// SwapBinary replaces the installed binary with the file at
// newBinary (same directory), keeping the previous binary at
// BackupPath. The returned rollback restores the previous
// binary. The caller stops the core first — swapping a running
// binary is never attempted.
func (m *Manager) SwapBinary(newBinary string) (rollback func() error, err error) {
	target := m.BinaryPath()
	bak := m.BackupPath()
	_ = os.Remove(bak)
	if m.Installed() {
		if err := os.Rename(target, bak); err != nil {
			return nil, fmt.Errorf("备份当前核心失败：%w", err)
		}
	}
	if err := os.Rename(newBinary, target); err != nil {
		_ = os.Rename(bak, target)
		return nil, fmt.Errorf("安装新核心失败：%w", err)
	}
	m.InvalidateVersionMemo()
	return func() error {
		_ = os.Remove(target)
		if rerr := os.Rename(bak, target); rerr != nil {
			return rerr
		}
		m.InvalidateVersionMemo()
		return nil
	}, nil
}

// minimalConfig renders the loopback-only starter configuration.
// The controller port is an ephemeral loopback port chosen at
// start time; there is deliberately no proxy listener (no
// mixed/socks/http port), no TUN and no DNS hijack — this stage
// proves lifecycle only; real proxying configuration arrives
// with the run-mode work.
func minimalConfig(controllerAddr, secret string) string {
	return fmt.Sprintf(`# DeBrowser 生成的 Mihomo 最小配置（仅本机控制器，无代理监听）
mode: rule
log-level: info
ipv6: false
external-controller: %s
secret: %q
proxies: []
proxy-groups: []
rules:
  - MATCH,DIRECT
`, controllerAddr, secret)
}

func freeLoopbackAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().String(), nil
}

func randomSecret() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (m *Manager) launchExec(binary, homeDir, configPath string, stdout io.Writer) (process, error) {
	cmd := exec.Command(binary, "-d", homeDir, "-f", configPath)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execProcess{cmd: cmd}, nil
}

// Start launches the core with the minimal configuration. The
// caller must have ensured installation.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.proc != nil {
		m.mu.Unlock()
		return fmt.Errorf("Mihomo 核心已在运行")
	}
	m.mu.Unlock()
	if err := m.EnsureInstalled(); err != nil {
		return err
	}
	home := m.HomeDir()
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("创建核心工作目录失败：%w", err)
	}
	addr, err := freeLoopbackAddr()
	if err != nil {
		return fmt.Errorf("分配本机控制器端口失败：%w", err)
	}
	secret, err := randomSecret()
	if err != nil {
		return fmt.Errorf("生成控制器密钥失败：%w", err)
	}
	configPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(configPath, []byte(minimalConfig(addr, secret)), 0o600); err != nil {
		return fmt.Errorf("写入核心配置失败：%w", err)
	}
	if err := os.MkdirAll(m.layout.Logs, 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败：%w", err)
	}
	logFile, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("打开核心日志失败：%w", err)
	}
	proc, err := m.launch(m.BinaryPath(), home, configPath, logFile)
	if err != nil {
		_ = logFile.Close()
		return fmt.Errorf("启动 Mihomo 核心失败：%w", err)
	}
	done := make(chan struct{})
	m.mu.Lock()
	m.proc = proc
	m.done = done
	m.startedAt = time.Now()
	m.logFile = logFile
	m.mu.Unlock()
	m.log.Info("Mihomo 核心已启动", "pid", proc.Pid())
	go func() {
		werr := proc.Wait()
		m.mu.Lock()
		if werr != nil {
			m.lastExit = fmt.Sprintf("异常退出：%v", werr)
		} else {
			m.lastExit = "已退出（退出码 0）"
		}
		m.proc = nil
		m.done = nil
		if m.logFile != nil {
			_ = m.logFile.Close()
			m.logFile = nil
		}
		m.mu.Unlock()
		close(done)
		m.log.Info("Mihomo 核心已退出", "result", m.lastExitSnapshot())
	}()
	return nil
}

func (m *Manager) lastExitSnapshot() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastExit
}

// Stop terminates the core process and waits for it to exit.
// Stopping a core that is not running is a no-op.
func (m *Manager) Stop() error {
	m.mu.Lock()
	proc, done := m.proc, m.done
	m.mu.Unlock()
	if proc == nil {
		return nil
	}
	if err := proc.Kill(); err != nil {
		return fmt.Errorf("停止 Mihomo 核心失败：%w", err)
	}
	if done != nil {
		<-done
	}
	return nil
}

// Shutdown is the application-exit cleanup: stop the core if it
// is running so no orphan process survives DeBrowser.
func (m *Manager) Shutdown() {
	_ = m.Stop()
}

// Status snapshots the core state.
func (m *Manager) Status(ctx context.Context) Status {
	st := Status{Installed: m.Installed()}
	if st.Installed {
		if v, err := m.Version(ctx); err == nil {
			st.Version = v
		}
	}
	m.mu.Lock()
	if m.proc != nil {
		st.Running = true
		st.PID = m.proc.Pid()
		st.StartedAt = m.startedAt.UTC().Format("2006-01-02 15:04:05")
	}
	st.LastExit = m.lastExit
	m.mu.Unlock()
	return st
}

// TailLog returns the last n lines of the core's own log file.
func (m *Manager) TailLog(n int) ([]string, error) {
	data, err := os.ReadFile(m.LogPath())
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("读取核心日志失败：%w", err)
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return []string{}, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}
