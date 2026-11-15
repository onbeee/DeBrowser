package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/onbeee/DeBrowser/internal/browserprobe"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/kernels"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/store"
)

// BrowserService is the Stage 9A browser and kernel management
// service exposed to the frontend over Wails bindings.
//
// Product decision (revised): DeBrowser does NOT detect browsers
// automatically — no registry scanning, no disk scanning, no
// auto-discovery of any kind. A browser becomes available only
// when the user explicitly adds it, in one of two ways:
//
//   - Manual: the user picks an installed Google Chrome or Brave
//     executable in the file picker. The file passes the full
//     identity/version/Authenticode-publisher verification and
//     the registration is persisted (migration 0011). DeBrowser
//     never copies, modifies, uninstalls or deletes the browser
//     or reads its everyday profile; removing a registration
//     deletes only the database row.
//   - Managed: the user explicitly downloads a Chrome for
//     Testing build that DeBrowser installs under
//     kernels/<version> (migration 0010). Only managed kernels
//     can be deleted from disk, through the guarded path here.
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
	inspector browserprobe.Inspector
	manager   *kernels.Manager
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
// whether manually registered or DeBrowser-managed.
type BrowserView struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Version         string `json:"version"`
	ExecutablePath  string `json:"executablePath"`
	Source          string `json:"source"` // "manual" | "managed"
	SignatureStatus string `json:"signatureStatus"`
	Publisher       string `json:"publisher"`
	DetectionStatus string `json:"detectionStatus"`
	StatusReason    string `json:"statusReason"`
	LastVerifiedAt  string `json:"lastVerifiedAt"`
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
		inspector: insp,
		manager:   &kernels.Manager{Fetcher: kernels.HTTPFetcher{}, Inspector: insp},
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
		LastVerifiedAt: k.UpdatedAt,
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

// manualView renders one persisted manual registration. If the
// registered file has disappeared, the view reports it as invalid
// (the stored record is updated on the next explicit verify).
func (s *BrowserService) manualView(b model.ExternalBrowser) BrowserView {
	v := BrowserView{
		ID:              b.ID,
		Name:            b.Name,
		Kind:            b.BrowserType,
		Version:         b.Version,
		ExecutablePath:  b.ExecutablePath,
		Source:          "manual",
		SignatureStatus: b.SignatureStatus,
		Publisher:       b.SignaturePublisher,
		LastVerifiedAt:  b.LastVerifiedAt,
	}
	switch {
	case !s.inspector.Exists(b.ExecutablePath):
		v.DetectionStatus = browserprobe.DetectionVerificationFailed
		v.StatusReason = "浏览器文件不存在（可能已被移动或卸载）"
	case b.Status == "verified":
		v.DetectionStatus = browserprobe.DetectionVerified
	default:
		v.DetectionStatus = browserprobe.DetectionVerificationFailed
		v.StatusReason = "最近一次验证未通过"
	}
	return v
}

// AddBrowserManually lets the user point at a browser executable
// through the native file picker. The chosen file must pass the
// full verification (identity from its version resource, version,
// Authenticode signature and publisher) and must be Google Chrome
// Stable or Brave Stable; anything else is rejected with the
// reason and nothing is recorded. A verified browser is persisted
// so it survives restarts. Adding the same executable twice
// refreshes its registration instead of duplicating it.
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
	info := browserprobe.Verify(path, s.inspector)
	if info.DetectionStatus != browserprobe.DetectionVerified {
		err = fmt.Errorf("该文件未通过浏览器验证：%s", info.StatusReason)
		return BrowserView{}, err
	}
	if info.Kind != browserprobe.KindChrome && info.Kind != browserprobe.KindBrave {
		err = fmt.Errorf("仅支持手动添加 Google Chrome 或 Brave，该文件是 %s", info.Name)
		return BrowserView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	record := model.ExternalBrowser{
		Name:               info.Name,
		BrowserType:        string(info.Kind),
		Version:            info.Version,
		ExecutablePath:     info.ExecutablePath,
		SignatureStatus:    info.SignatureStatus,
		SignaturePublisher: info.Publisher,
		Status:             "verified",
		LastVerifiedAt:     info.VerifiedAt,
	}
	if existing, gerr := s.store.GetExternalBrowserByPath(ctx, info.ExecutablePath); gerr == nil {
		record.ID = existing.ID
		record.CreatedAt = existing.CreatedAt
		if uerr := s.store.UpdateExternalBrowserVerification(ctx, record); uerr != nil {
			err = uerr
			return BrowserView{}, err
		}
		out = s.manualView(record)
		return out, nil
	}
	created, cerr := s.store.CreateExternalBrowser(ctx, record)
	if cerr != nil {
		err = cerr
		return BrowserView{}, err
	}
	out = s.manualView(created)
	return out, nil
}

// reverify re-checks one manual registration against the real
// file (existence, identity, version, signature, publisher),
// persists the outcome and returns the fresh view.
func (s *BrowserService) reverify(ctx context.Context, b model.ExternalBrowser) (BrowserView, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if !s.inspector.Exists(b.ExecutablePath) {
		b.Status = "invalid"
		b.SignatureStatus = browserprobe.SignatureUnavailable
		b.LastVerifiedAt = now
		if err := s.store.UpdateExternalBrowserVerification(ctx, b); err != nil {
			return BrowserView{}, err
		}
		v := s.manualView(b)
		v.StatusReason = "浏览器文件不存在（可能已被移动或卸载）"
		return v, nil
	}
	info := browserprobe.Verify(b.ExecutablePath, s.inspector)
	b.Name = info.Name
	b.Version = info.Version
	b.SignatureStatus = info.SignatureStatus
	b.SignaturePublisher = info.Publisher
	b.LastVerifiedAt = info.VerifiedAt
	if info.DetectionStatus == browserprobe.DetectionVerified && string(info.Kind) == b.BrowserType {
		b.Status = "verified"
	} else {
		b.Status = "invalid"
	}
	if err := s.store.UpdateExternalBrowserVerification(ctx, b); err != nil {
		return BrowserView{}, err
	}
	v := s.manualView(b)
	if b.Status != "verified" {
		v.StatusReason = info.StatusReason
		if info.DetectionStatus == browserprobe.DetectionVerified && string(info.Kind) != b.BrowserType {
			v.StatusReason = "浏览器类型与登记记录不一致"
		}
	}
	return v, nil
}

// VerifyBrowser re-verifies one manually registered browser and
// records the outcome.
func (s *BrowserService) VerifyBrowser(id string) (BrowserView, error) {
	start := time.Now()
	var out BrowserView
	var err error
	defer func() { auditOp(s.Auditor, "browser.verify", "browser", id, start, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, gerr := s.store.GetExternalBrowser(ctx, id)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该浏览器登记")
		} else {
			err = gerr
		}
		return BrowserView{}, err
	}
	out, err = s.reverify(ctx, b)
	return out, err
}

// RemoveBrowserRegistration removes one manual browser
// registration. It deletes only the database record — the browser
// file itself is never deleted, moved or modified.
func (s *BrowserService) RemoveBrowserRegistration(id string) error {
	start := time.Now()
	var err error
	defer func() { auditOp(s.Auditor, "browser.remove_registration", "browser", id, start, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if derr := s.store.DeleteExternalBrowser(ctx, id); derr != nil {
		if errors.Is(derr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该浏览器登记")
		} else {
			err = derr
		}
		return err
	}
	return nil
}

// ListBrowsers returns the unified list: the user's persisted
// manual registrations plus all managed kernels. Nothing is
// auto-detected; both parts come from explicit user actions.
func (s *BrowserService) ListBrowsers() ([]BrowserView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := []BrowserView{}
	bs, err := s.store.ListExternalBrowsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取浏览器登记失败：%w", err)
	}
	for _, b := range bs {
		out = append(out, s.manualView(b))
	}
	ks, err := s.store.ListKernels(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取内核列表失败：%w", err)
	}
	for _, k := range ks {
		out = append(out, s.managedView(k))
	}
	return out, nil
}

// GetBrowserDetails returns one browser by ID. A manual
// registration is re-verified live before it is shown (and the
// outcome persisted); a managed kernel is returned from its
// registration.
func (s *BrowserService) GetBrowserDetails(id string) (BrowserView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if b, err := s.store.GetExternalBrowser(ctx, id); err == nil {
		return s.reverify(ctx, b)
	} else if !errors.Is(err, store.ErrNotFound) {
		return BrowserView{}, err
	}
	k, err := s.store.GetKernel(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BrowserView{}, fmt.Errorf("未找到该浏览器记录")
		}
		return BrowserView{}, err
	}
	return s.managedView(k), nil
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
// its registration. Manually registered system browsers are not
// kernels and can never be deleted through this method. If the
// registration delete fails after the files are gone, the row is
// marked missing so the database and the disk stay consistent.
func (s *BrowserService) DeleteManagedKernel(id string) error {
	start := time.Now()
	var err error
	defer func() { auditOp(s.Auditor, "kernel.delete", "kernel", id, start, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k, gerr := s.store.GetKernel(ctx, id)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该内核（手动添加的浏览器请使用移除登记）")
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
		_ = s.store.UpdateKernelStatus(ctx, id, "missing")
		err = fmt.Errorf("删除内核登记失败：%w", derr)
		return err
	}
	return nil
}
