package service

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/mihomocore"
)

// CoreService exposes the Mihomo core lifecycle and the manual
// kernel update flow to the settings page. Update checks happen
// ONLY when the user clicks the check button — nothing here
// runs on a timer or at startup — and an update is installed
// only after the user clicks update and the downloaded asset's
// SHA-256 matches the digest published in the official release.
type CoreService struct {
	core *mihomocore.Manager
	log  *slog.Logger

	// Seams for tests.
	apiBase    string
	httpClient *http.Client
	versionFn  func(ctx context.Context, binary string) (string, error)

	mu              sync.Mutex
	checkStatus     string // "" | up-to-date | update-available | check-failed
	latestVersion   string
	checkedAt       string
	checkError      string
	assetURL        string
	assetSHA256     string
	lastUpdateError string
}

// NewCoreService builds the service over the core manager.
func NewCoreService(core *mihomocore.Manager, log *slog.Logger) *CoreService {
	if log == nil {
		log = slog.Default()
	}
	return &CoreService{
		core:       core,
		log:        log,
		apiBase:    mihomocore.GitHubAPIRepo,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// CoreStatusView is the settings page's core status.
type CoreStatusView struct {
	Installed bool   `json:"installed"`
	Version   string `json:"version"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"`
	LastExit  string `json:"lastExit"`
}

// GetCoreStatus returns the core's current state; the version
// comes from running the actual installed binary.
func (s *CoreService) GetCoreStatus() CoreStatusView {
	c, cancel := ctx()
	defer cancel()
	st := s.core.Status(c)
	return CoreStatusView{
		Installed: st.Installed,
		Version:   st.Version,
		Running:   st.Running,
		PID:       st.PID,
		StartedAt: st.StartedAt,
		LastExit:  st.LastExit,
	}
}

// StartCore starts the core with the minimal loopback config.
func (s *CoreService) StartCore() error {
	c, cancel := ctx()
	defer cancel()
	return s.core.Start(c)
}

// StopCore stops the core.
func (s *CoreService) StopCore() error {
	return s.core.Stop()
}

// GetCoreLog returns the tail of the core's own log.
func (s *CoreService) GetCoreLog(lines int) ([]string, error) {
	if lines <= 0 || lines > 500 {
		lines = 200
	}
	return s.core.TailLog(lines)
}

// CoreUpdateView is the settings page's update state.
type CoreUpdateView struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	Status         string `json:"status"` // unchecked | up-to-date | update-available | check-failed
	CheckedAt      string `json:"checkedAt"`
	Error          string `json:"error"`
}

// versionOf runs the version query against an arbitrary
// binary (the seam replaces it in tests).
func (s *CoreService) versionOf(c context.Context, binary string) (string, error) {
	if s.versionFn != nil {
		return s.versionFn(c, binary)
	}
	return s.core.VersionOf(c, binary)
}

// currentVersion is the installed core's version (memoised by
// the manager unless a test seam is installed).
func (s *CoreService) currentVersion(c context.Context) (string, error) {
	if s.versionFn != nil {
		return s.versionFn(c, s.core.BinaryPath())
	}
	return s.core.Version(c)
}

// GetCoreUpdate returns the current update-check state.
func (s *CoreService) GetCoreUpdate() CoreUpdateView {
	c, cancel := ctx()
	defer cancel()
	current, _ := s.currentVersion(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.checkStatus
	if status == "" {
		status = "unchecked"
	}
	errText := s.checkError
	if errText == "" {
		errText = s.lastUpdateError
	}
	return CoreUpdateView{
		CurrentVersion: current,
		LatestVersion:  s.latestVersion,
		Status:         status,
		CheckedAt:      s.checkedAt,
		Error:          errText,
	}
}

type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Digest             string `json:"digest"`
	} `json:"assets"`
}

// CheckCoreUpdate queries the official GitHub Releases API for
// the latest stable release. User-triggered only.
func (s *CoreService) CheckCoreUpdate() (CoreUpdateView, error) {
	c, cancel := ctx()
	defer cancel()
	fail := func(err error) (CoreUpdateView, error) {
		s.mu.Lock()
		s.checkStatus = "check-failed"
		s.checkError = err.Error()
		s.checkedAt = time.Now().Format("2006-01-02 15:04:05")
		s.mu.Unlock()
		return s.GetCoreUpdate(), err
	}
	req, err := http.NewRequestWithContext(c, http.MethodGet, s.apiBase+"/releases/latest", nil)
	if err != nil {
		return fail(fmt.Errorf("检查更新失败：%w", err))
	}
	req.Header.Set("User-Agent", "DeBrowser")
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fail(fmt.Errorf("检查更新失败：无法访问 GitHub 发布服务"))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("检查更新失败：GitHub 发布服务返回 HTTP %d", resp.StatusCode))
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return fail(fmt.Errorf("检查更新失败：发布信息格式无效"))
	}
	if rel.Prerelease || rel.TagName == "" {
		return fail(fmt.Errorf("检查更新失败：未找到正式稳定版本"))
	}
	wantAsset := "mihomo-windows-amd64-" + rel.TagName + ".zip"
	var assetURL, assetDigest string
	for _, a := range rel.Assets {
		if a.Name == wantAsset {
			assetURL = a.BrowserDownloadURL
			assetDigest = strings.TrimPrefix(a.Digest, "sha256:")
		}
	}
	if assetURL == "" || assetDigest == "" {
		return fail(fmt.Errorf("检查更新失败：官方版本 %s 缺少 Windows x64 资产或其 SHA-256", rel.TagName))
	}
	current, _ := s.currentVersion(c)
	s.mu.Lock()
	s.latestVersion = rel.TagName
	s.assetURL = assetURL
	s.assetSHA256 = assetDigest
	s.checkedAt = time.Now().Format("2006-01-02 15:04:05")
	s.checkError = ""
	if current != "" && compareVersions(current, rel.TagName) >= 0 {
		s.checkStatus = "up-to-date"
	} else {
		s.checkStatus = "update-available"
	}
	s.mu.Unlock()
	s.log.Info("Mihomo 内核更新检查完成", "current", current, "latest", rel.TagName)
	return s.GetCoreUpdate(), nil
}

// compareVersions compares two "vX.Y.Z" tags numerically.
func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	for i := 0; i < len(parts) && i < 3; i++ {
		n, err := strconv.Atoi(parts[i])
		if err == nil {
			out[i] = n
		}
	}
	return out
}

// UpdateCore downloads the latest official asset found by the
// last successful check, verifies its SHA-256 against the
// digest from the official release, verifies the extracted
// binary reports the target version, and only then swaps it
// in — stopping and restarting the core around the swap when
// it was running. Any failure leaves the previous core fully
// intact (and running, if it was).
func (s *CoreService) UpdateCore() (CoreUpdateView, error) {
	s.mu.Lock()
	assetURL, wantSHA, target := s.assetURL, s.assetSHA256, s.latestVersion
	status := s.checkStatus
	s.mu.Unlock()
	fail := func(err error) (CoreUpdateView, error) {
		s.mu.Lock()
		s.lastUpdateError = err.Error()
		s.mu.Unlock()
		s.log.Error("Mihomo 内核更新失败", "error", err)
		return s.GetCoreUpdate(), err
	}
	if status != "update-available" || assetURL == "" {
		return fail(fmt.Errorf("没有可安装的更新，请先检查更新"))
	}
	c, cancel := ctx()
	defer cancel()

	dir := filepath.Dir(s.core.BinaryPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(fmt.Errorf("更新失败：%w", err))
	}
	zipPath := filepath.Join(dir, ".update-download.zip")
	exePath := filepath.Join(dir, ".update-mihomo.exe")
	_ = os.Remove(zipPath)
	_ = os.Remove(exePath)
	defer func() { _ = os.Remove(zipPath) }()

	// 1. Download to a temp file, hashing as we go.
	if err := s.downloadAsset(c, assetURL, zipPath); err != nil {
		return fail(err)
	}
	sum, err := fileSHA256(zipPath)
	if err != nil {
		return fail(fmt.Errorf("更新失败：%w", err))
	}
	if !strings.EqualFold(sum, wantSHA) {
		_ = os.Remove(exePath)
		return fail(fmt.Errorf("更新失败：下载文件的 SHA-256 与官方发布不一致，已拒绝安装"))
	}
	// 2. Extract the single official exe.
	if err := extractCoreExe(zipPath, exePath); err != nil {
		return fail(err)
	}
	defer func() { _ = os.Remove(exePath) }()
	// 3. The extracted binary must actually report the target
	// version before anything is replaced.
	gotVersion, err := s.versionOf(c, exePath)
	if err != nil || gotVersion != target {
		return fail(fmt.Errorf("更新失败：新核心版本校验不通过（期望 %s，实际 %q）", target, gotVersion))
	}
	// 4. Swap, with the previous binary kept for rollback.
	st := s.core.Status(c)
	wasRunning := st.Running
	if wasRunning {
		if err := s.core.Stop(); err != nil {
			return fail(fmt.Errorf("更新失败：%w", err))
		}
	}
	rollback, err := s.core.SwapBinary(exePath)
	if err != nil {
		if wasRunning {
			_ = s.core.Start(c)
		}
		return fail(fmt.Errorf("更新失败：%w", err))
	}
	// 5. Verify the installed binary, restart if it ran before.
	installed, verr := s.versionOf(c, s.core.BinaryPath())
	if verr != nil || installed != target {
		_ = rollback()
		if wasRunning {
			_ = s.core.Start(c)
		}
		return fail(fmt.Errorf("更新失败：安装后版本校验不通过，已恢复原核心"))
	}
	if wasRunning {
		if err := s.core.Start(c); err != nil {
			_ = rollback()
			_ = s.core.Start(c)
			return fail(fmt.Errorf("更新失败：新核心启动失败，已恢复原核心"))
		}
	}
	s.mu.Lock()
	s.checkStatus = "up-to-date"
	s.lastUpdateError = ""
	s.mu.Unlock()
	s.log.Info("Mihomo 内核更新完成", "version", target)
	return s.GetCoreUpdate(), nil
}

func (s *CoreService) downloadAsset(c context.Context, url, dst string) error {
	dlCtx, cancel := context.WithTimeout(c, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("更新失败：下载地址无效")
	}
	req.Header.Set("User-Agent", "DeBrowser")
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("更新失败：下载中断")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("更新失败：下载服务返回 HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("更新失败：%w", err)
	}
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 256<<20)); err != nil {
		_ = f.Close()
		return fmt.Errorf("更新失败：下载中断")
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("更新失败：%w", err)
	}
	return f.Close()
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractCoreExe pulls the single .exe out of the official zip,
// rejecting archives with paths or unexpected contents.
func extractCoreExe(zipPath, dst string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("更新失败：发布压缩包无效")
	}
	defer func() { _ = r.Close() }()
	var exe *zip.File
	for _, f := range r.File {
		name := filepath.Base(f.Name)
		if f.Name != name || !strings.HasSuffix(strings.ToLower(name), ".exe") {
			return fmt.Errorf("更新失败：发布压缩包内容不符合官方结构")
		}
		if exe != nil {
			return fmt.Errorf("更新失败：发布压缩包内容不符合官方结构")
		}
		exe = f
	}
	if exe == nil {
		return fmt.Errorf("更新失败：发布压缩包中没有核心程序")
	}
	rc, err := exe.Open()
	if err != nil {
		return fmt.Errorf("更新失败：无法读取发布压缩包")
	}
	defer func() { _ = rc.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return fmt.Errorf("更新失败：%w", err)
	}
	if _, err := io.Copy(out, rc); err != nil {
		_ = out.Close()
		return fmt.Errorf("更新失败：解压核心失败")
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return fmt.Errorf("更新失败：%w", err)
	}
	return out.Close()
}

// CoreFaultInfo is the core's contribution to the diagnostics
// fault report (§10): versions and lifecycle state only — never
// the controller secret or any user data.
type CoreFaultInfo struct {
	Installed       bool
	Version         string
	Running         bool
	LastExit        string
	UpdateStatus    string
	LatestVersion   string
	LastUpdateError string
}

// CoreFaultInfo snapshots the core state for the fault report.
func (s *CoreService) CoreFaultInfo() CoreFaultInfo {
	c, cancel := ctx()
	defer cancel()
	st := s.core.Status(c)
	s.mu.Lock()
	defer s.mu.Unlock()
	return CoreFaultInfo{
		Installed:       st.Installed,
		Version:         st.Version,
		Running:         st.Running,
		LastExit:        st.LastExit,
		UpdateStatus:    s.checkStatus,
		LatestVersion:   s.latestVersion,
		LastUpdateError: s.lastUpdateError,
	}
}
