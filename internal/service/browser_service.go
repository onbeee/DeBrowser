package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/browserprobe"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/kernels"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/store"
)

// BrowserService is the Stage 9A browser discovery and kernel
// management service exposed to the frontend over Wails bindings.
//
// Two kinds of browsers are presented through one view type:
//   - System browsers (Google Chrome, Brave) that the user already
//     has installed. They are detected live on every call, are
//     never written to the database, and this service offers no
//     way to modify, copy, uninstall or delete them or to touch
//     their profiles.
//   - Managed kernels: Chrome for Testing builds DeBrowser itself
//     downloaded, verified and installed under kernels/<version>.
//     Only these are persisted (migration 0010) and only these can
//     be deleted, through the guarded path in this service.
//
// This stage never launches a browser process.
type BrowserService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger

	// Auditor, when set, receives operation-audit records for
	// user-initiated mutations (Stage 5B pattern).
	Auditor *logging.System

	// ExecutablePicker opens the native file picker for manual
	// browser registration. It is injected by the app package (the
	// service layer stays GUI-free, like the diagnostics save
	// picker). Nil means manual adding is unavailable.
	ExecutablePicker func() (string, error)

	// OnProgress, when set, receives managed-kernel install
	// progress for forwarding to the frontend. Injected by the app
	// package.
	OnProgress func(InstallProgress)

	// Platform seams (production values set by the constructor;
	// tests substitute fakes).
	sources   []browserprobe.CandidateSource
	inspector browserprobe.Inspector
	manager   *kernels.Manager

	mu     sync.Mutex
	manual []browserprobe.Info // session-only manual additions, never persisted
}

// InstallProgress is one managed-kernel install progress update.
type InstallProgress struct {
	Version string `json:"version"`
	Phase   string `json:"phase"` // downloading | extracting | verifying | installing | done | failed
	Percent int    `json:"percent"`
}

// InstallProgressEvent is the Wails event name for InstallProgress.
const InstallProgressEvent = "kernel:install-progress"

// BrowserView is the unified presentation record for one browser,
// whether system-detected or DeBrowser-managed.
type BrowserView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Version         string `json:"version"`
	ExecutablePath  string `json:"executablePath"`
	Source          string `json:"source"` // "system" | "managed"
	Scope           string `json:"scope"`  // "user" | "system" | "managed"
	SignatureStatus string `json:"signatureStatus"`
	Publisher       string `json:"publisher"`
	DetectionStatus string `json:"detectionStatus"`
	StatusReason    string `json:"statusReason"`
	DetectedAt      string `json:"detectedAt"`
}

// KernelView is one managed kernel with its resolved absolute
// paths for display (the database stores portable relative paths).
type KernelView struct {
	model.Kernel
	AbsInstallPath    string `json:"absInstallPath"`
	AbsExecutablePath string `json:"absExecutablePath"`
}

// StableVersionView describes the official Stable Chrome for
// Testing build currently offered for download.
type StableVersionView struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// NewBrowserService builds the service with production platform
// implementations.
func NewBrowserService(db *database.DB, layout paths.Layout, log *slog.Logger) *BrowserService {
	insp := browserprobe.NewInspector()
	return &BrowserService{
		store:     store.New(db),
		layout:    layout,
		log:       log,
		sources:   browserprobe.Sources(),
		inspector: insp,
		manager:   &kernels.Manager{Fetcher: kernels.HTTPFetcher{}, Inspector: insp},
	}
}

func browserViewFromInfo(info browserprobe.Info) BrowserView {
	return BrowserView{
		ID:              info.ID,
		Name:            info.Name,
		Kind:            string(info.Kind),
		Version:         info.Version,
		ExecutablePath:  info.ExecutablePath,
		Source:          info.Source,
		Scope:           string(info.Scope),
		SignatureStatus: info.SignatureStatus,
		Publisher:       info.Publisher,
		DetectionStatus: info.DetectionStatus,
		StatusReason:    info.StatusReason,
		DetectedAt:      info.DetectedAt,
	}
}

func (s *BrowserService) kernelView(k model.Kernel) KernelView {
	return KernelView{
		Kernel:            k,
		AbsInstallPath:    filepath.Join(s.layout.Root, k.InstallPath),
		AbsExecutablePath: filepath.Join(s.layout.Root, k.ExecutablePath),
	}
}

func (s *BrowserService) managedView(k model.Kernel) BrowserView {
	v := BrowserView{
		ID:             k.ID,
		Name:           "Chromium（DeBrowser 管理）",
		Kind:           string(browserprobe.KindChromium),
		Version:        k.Version,
		ExecutablePath: filepath.Join(s.layout.Root, k.ExecutablePath),
		Source:         "managed",
		Scope:          "managed",
		DetectedAt:     k.UpdatedAt,
	}
	switch k.Status {
	case "ready":
		v.DetectionStatus = browserprobe.DetectionVerified
		v.SignatureStatus = browserprobe.SignatureVerified
		v.Publisher = browserprobe.ExpectedPublisher(browserprobe.KindChromium)
	case "missing":
		v.DetectionStatus = browserprobe.DetectionVerificationFailed
		v.SignatureStatus = browserprobe.SignatureUnavailable
		v.StatusReason = "内核文件缺失"
	default: // verification_failed
		v.DetectionStatus = browserprobe.DetectionVerificationFailed
		v.SignatureStatus = browserprobe.SignatureInvalid
		v.StatusReason = "内核验证未通过"
	}
	return v
}

// detectSystem runs live detection and merges the session's manual
// additions (deduplicated by executable path).
func (s *BrowserService) detectSystem() []browserprobe.Info {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	found := browserprobe.Detect(ctx, s.sources, s.inspector)
	s.mu.Lock()
	manual := append([]browserprobe.Info(nil), s.manual...)
	s.mu.Unlock()
	seen := map[string]bool{}
	for _, f := range found {
		seen[strings.ToLower(f.ExecutablePath)] = true
	}
	for _, m := range manual {
		if !seen[strings.ToLower(m.ExecutablePath)] {
			found = append(found, m)
		}
	}
	return found
}

// DetectSystemBrowsers detects the installed system browsers
// (Chrome, Brave) without persisting anything.
func (s *BrowserService) DetectSystemBrowsers() ([]BrowserView, error) {
	out := []BrowserView{}
	for _, info := range s.detectSystem() {
		out = append(out, browserViewFromInfo(info))
	}
	return out, nil
}

// RedetectBrowsers re-runs system browser detection on explicit
// user request. Detection is live by construction, so this is the
// same fresh scan; the separate method keeps the UI action (and
// its audit semantics) explicit.
func (s *BrowserService) RedetectBrowsers() ([]BrowserView, error) {
	return s.DetectSystemBrowsers()
}

// ListBrowsers returns the unified list: live-detected system
// browsers plus all managed kernels.
func (s *BrowserService) ListBrowsers() ([]BrowserView, error) {
	out, err := s.DetectSystemBrowsers()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ks, err := s.store.ListKernels(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取内核列表失败：%w", err)
	}
	for _, k := range ks {
		out = append(out, s.managedView(k))
	}
	return out, nil
}

// GetBrowserDetails returns one browser by ID: a system detection
// record (re-detected live) or a managed kernel.
func (s *BrowserService) GetBrowserDetails(id string) (BrowserView, error) {
	for _, info := range s.detectSystem() {
		if info.ID == id {
			return browserViewFromInfo(info), nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	k, err := s.store.GetKernel(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BrowserView{}, fmt.Errorf("未找到该浏览器记录")
		}
		return BrowserView{}, err
	}
	return s.managedView(k), nil
}

// AddBrowserManually lets the user point at a browser executable
// through the native file picker. The chosen file goes through the
// exact same identity, version and signature verification as an
// auto-detected one; a file that does not verify is rejected and
// never registered. A verified browser is remembered for this
// session only — system browsers are not persisted.
func (s *BrowserService) AddBrowserManually() (BrowserView, error) {
	start := time.Now()
	var out BrowserView
	var err error
	defer func() { auditOp(s.Auditor, "browser.add_manual", "browser", out.ID, start, err) }()
	if s.ExecutablePicker == nil {
		err = fmt.Errorf("文件选择器不可用")
		return BrowserView{}, err
	}
	path, perr := s.ExecutablePicker()
	if perr != nil {
		err = perr
		return BrowserView{}, err
	}
	if path == "" {
		err = fmt.Errorf("未选择文件")
		return BrowserView{}, err
	}
	if !s.inspector.Exists(path) {
		err = fmt.Errorf("所选文件不存在")
		return BrowserView{}, err
	}
	info := browserprobe.Verify(path, browserprobe.ScopeUser, s.inspector)
	if info.DetectionStatus != browserprobe.DetectionVerified {
		err = fmt.Errorf("该文件未通过浏览器验证：%s", info.StatusReason)
		return BrowserView{}, err
	}
	s.mu.Lock()
	replaced := false
	for i, m := range s.manual {
		if strings.EqualFold(m.ExecutablePath, info.ExecutablePath) {
			s.manual[i] = info
			replaced = true
		}
	}
	if !replaced {
		s.manual = append(s.manual, info)
	}
	s.mu.Unlock()
	out = browserViewFromInfo(info)
	return out, nil
}

// GetOfficialStableVersion fetches the official Chrome for Testing
// metadata and returns the current Stable build for this platform.
func (s *BrowserService) GetOfficialStableVersion() (StableVersionView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	body, err := s.manager.Fetcher.Get(ctx, kernels.MetadataURL)
	if err != nil {
		return StableVersionView{}, fmt.Errorf("获取官方版本信息失败：%w", err)
	}
	rel, err := kernels.ParseStableRelease(body)
	if err != nil {
		return StableVersionView{}, fmt.Errorf("解析官方版本信息失败：%w", err)
	}
	return StableVersionView{Version: rel.Version, Platform: kernels.Platform}, nil
}

// InstallManagedKernel downloads, verifies and installs the
// current official Stable Chromium as a DeBrowser-managed kernel,
// then registers it. If registration fails after installation, the
// installed directory is removed again so disk and database never
// disagree.
func (s *BrowserService) InstallManagedKernel() (KernelView, error) {
	start := time.Now()
	var out KernelView
	var err error
	defer func() { auditOp(s.Auditor, "kernel.install", "kernel", out.ID, start, err) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	body, ferr := s.manager.Fetcher.Get(ctx, kernels.MetadataURL)
	cancel()
	if ferr != nil {
		err = fmt.Errorf("获取官方版本信息失败：%w", ferr)
		return KernelView{}, err
	}
	rel, perr := kernels.ParseStableRelease(body)
	if perr != nil {
		err = fmt.Errorf("解析官方版本信息失败：%w", perr)
		return KernelView{}, err
	}

	emit := func(phase string, percent int) {
		if s.OnProgress != nil {
			s.OnProgress(InstallProgress{Version: rel.Version, Phase: phase, Percent: percent})
		}
	}
	dir, ierr := s.manager.Install(context.Background(), s.layout.Kernels, rel, emit)
	if ierr != nil {
		if errors.Is(ierr, kernels.ErrAlreadyInstalled) {
			err = fmt.Errorf("该版本内核已经安装")
		} else {
			err = fmt.Errorf("安装内核失败：%w", ierr)
		}
		if s.OnProgress != nil {
			s.OnProgress(InstallProgress{Version: rel.Version, Phase: "failed", Percent: -1})
		}
		return KernelView{}, err
	}
	relInstall, rerr := filepath.Rel(s.layout.Root, dir)
	if rerr != nil {
		_ = kernels.RemoveManaged(s.layout.Kernels, rel.Version)
		err = fmt.Errorf("安装内核失败：%w", rerr)
		return KernelView{}, err
	}
	k, cerr := s.store.CreateKernel(context.Background(), model.Kernel{
		Version:        rel.Version,
		Channel:        "stable",
		Platform:       kernels.Platform,
		InstallPath:    relInstall,
		ExecutablePath: filepath.Join(relInstall, "chrome.exe"),
		Status:         "ready",
	})
	if cerr != nil {
		_ = kernels.RemoveManaged(s.layout.Kernels, rel.Version)
		err = fmt.Errorf("登记内核失败：%w", cerr)
		return KernelView{}, err
	}
	if s.OnProgress != nil {
		s.OnProgress(InstallProgress{Version: rel.Version, Phase: "done", Percent: 100})
	}
	out = s.kernelView(k)
	return out, nil
}

// ListManagedKernels returns every DeBrowser-managed kernel.
func (s *BrowserService) ListManagedKernels() ([]KernelView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ks, err := s.store.ListKernels(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取内核列表失败：%w", err)
	}
	out := []KernelView{}
	for _, k := range ks {
		out = append(out, s.kernelView(k))
	}
	return out, nil
}

// VerifyManagedKernel re-validates one managed kernel in place
// (files present, version match, signature) and records the
// outcome as its status.
func (s *BrowserService) VerifyManagedKernel(id string) (KernelView, error) {
	start := time.Now()
	var out KernelView
	var err error
	defer func() { auditOp(s.Auditor, "kernel.verify", "kernel", id, start, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k, gerr := s.store.GetKernel(ctx, id)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该内核")
		} else {
			err = gerr
		}
		return KernelView{}, err
	}
	status := "ready"
	dir, rerr := kernels.ResolveManagedDir(s.layout.Kernels, filepath.Base(k.InstallPath))
	if rerr != nil {
		status = "missing"
	} else if verr := s.manager.VerifyInstalled(dir, k.Version); verr != nil {
		status = "verification_failed"
	}
	if uerr := s.store.UpdateKernelStatus(ctx, id, status); uerr != nil {
		err = uerr
		return KernelView{}, err
	}
	k.Status = status
	out = s.kernelView(k)
	return out, nil
}

// DeleteManagedKernel removes one DeBrowser-managed kernel: the
// directory (after the full managed-path verification) and then
// its registration. System browsers have no registration, so this
// method can never delete one. If the registration delete fails
// after the files are gone, the registration is restored so the
// database and the disk stay consistent.
func (s *BrowserService) DeleteManagedKernel(id string) error {
	start := time.Now()
	var err error
	defer func() { auditOp(s.Auditor, "kernel.delete", "kernel", id, start, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k, gerr := s.store.GetKernel(ctx, id)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该内核（系统浏览器不能通过此操作删除）")
		} else {
			err = gerr
		}
		return err
	}
	if rerr := kernels.RemoveManaged(s.layout.Kernels, filepath.Base(k.InstallPath)); rerr != nil {
		err = fmt.Errorf("删除内核文件失败：%w", rerr)
		return err
	}
	if derr := s.store.DeleteKernel(ctx, id); derr != nil {
		// Files are gone but the row remains: try to keep the pair
		// consistent by reporting loudly; the row's status is set
		// to missing so the UI reflects the real disk state.
		_ = s.store.UpdateKernelStatus(ctx, id, "missing")
		err = fmt.Errorf("删除内核登记失败：%w", derr)
		return err
	}
	return nil
}
