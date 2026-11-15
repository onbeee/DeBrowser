package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
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
// Product decision: DeBrowser does NOT detect browsers
// automatically — no registry scanning, no disk scanning, no
// auto-discovery of any kind. A browser becomes available only
// when the user explicitly adds it, in one of two ways:
//
//   - Manual: the user picks a program executable in the file
//     picker and DeBrowser registers ONLY its path (migration
//     0011, reshaped by 0012). There is NO identity, version,
//     signature or publisher check on the chosen file — the
//     user chose it, and judging or launching it is not this
//     stage's business. DeBrowser never copies, modifies,
//     uninstalls or deletes the registered program or reads its
//     profile; removing a registration deletes only the
//     database row.
//   - Managed: the user explicitly downloads a Chrome for
//     Testing build that DeBrowser installs under
//     kernels/<version> (migration 0010) and verifies as part of
//     the installation. Only managed kernels can be deleted
//     from disk, through the guarded path here.
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

	// Platform seam (production value set by the constructor;
	// tests substitute a fake-backed manager).
	manager *kernels.Manager

	// installTimeout bounds one whole managed-kernel installation
	// (metadata fetch + download + extraction + verification +
	// registration) under a single controlled context.
	installTimeout time.Duration
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
		store:          store.New(db),
		layout:         layout,
		log:            log,
		manager:        &kernels.Manager{Fetcher: kernels.HTTPFetcher{}, Inspector: insp},
		installTimeout: 15 * time.Minute,
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

// manualView renders one persisted manual registration. It is a
// pure path record: no file access, no checks of any kind.
func (s *BrowserService) manualView(b model.ExternalBrowser) BrowserView {
	return BrowserView{
		ID:             b.ID,
		Name:           b.Name,
		ExecutablePath: b.ExecutablePath,
		Source:         "manual",
	}
}

// displayNameForPath derives a registration's display name from
// the chosen file's name (extension stripped). It is pure string
// handling of the path the user picked — the file itself is
// never opened, read or executed.
func displayNameForPath(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		if stem := strings.TrimSuffix(base, ext); stem != "" {
			return stem
		}
	}
	return base
}

// AddBrowserManually lets the user point at a program executable
// through the native file picker and registers ONLY the chosen
// path. There is deliberately NO check of any kind on the file:
// no identity, product, version, signature or publisher
// verification, and the file is not even required to be read —
// the picker already guarantees the user picked it deliberately.
// Registering the same path twice returns the existing
// registration instead of duplicating it.
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if existing, gerr := s.store.GetExternalBrowserByPath(ctx, path); gerr == nil {
		out = s.manualView(existing)
		return out, nil
	} else if !errors.Is(gerr, store.ErrNotFound) {
		err = gerr
		return BrowserView{}, err
	}
	created, cerr := s.store.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name:           displayNameForPath(path),
		ExecutablePath: path,
	})
	if cerr != nil {
		err = cerr
		return BrowserView{}, err
	}
	out = s.manualView(created)
	return out, nil
}

// UpdateBrowserPath re-points one manual registration at a newly
// picked executable path (the "重新选择路径" action). Like
// AddBrowserManually it performs no check on the chosen file; it
// only refuses to steal a path another registration already
// holds.
func (s *BrowserService) UpdateBrowserPath(id string) (BrowserView, error) {
	start := time.Now()
	var out BrowserView
	var err error
	defer func() { auditOp(s.Auditor, "browser.update_path", "browser", id, start, err) }()
	if s.ExecutablePicker == nil {
		err = fmt.Errorf("文件选择器不可用")
		return BrowserView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	path, perr := s.ExecutablePicker()
	if perr != nil {
		err = perr
		return BrowserView{}, err
	}
	if path == "" {
		err = fmt.Errorf("未选择文件")
		return BrowserView{}, err
	}
	if other, oerr := s.store.GetExternalBrowserByPath(ctx, path); oerr == nil && other.ID != id {
		err = fmt.Errorf("该路径已登记为其他浏览器")
		return BrowserView{}, err
	} else if oerr != nil && !errors.Is(oerr, store.ErrNotFound) {
		err = oerr
		return BrowserView{}, err
	}
	name := displayNameForPath(path)
	if uerr := s.store.UpdateExternalBrowserPath(ctx, id, name, path); uerr != nil {
		err = uerr
		return BrowserView{}, err
	}
	b.Name = name
	b.ExecutablePath = path
	out = s.manualView(b)
	return out, nil
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
// registration is returned exactly as stored (it is a pure path
// record — there is nothing to re-verify); a managed kernel is
// returned from its registration.
func (s *BrowserService) GetBrowserDetails(id string) (BrowserView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if b, err := s.store.GetExternalBrowser(ctx, id); err == nil {
		return s.manualView(b), nil
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

	// ONE controlled context governs the entire installation —
	// metadata fetch, ZIP download, extraction, verification and
	// registration. On timeout or cancellation the manager cleans
	// its temporary download/extraction files and nothing is
	// registered, so no half-installed, unregistered kernel can be
	// left behind.
	timeout := s.installTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	body, ferr := s.manager.Fetcher.Get(ctx, kernels.MetadataURL)
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
	dir, ierr := s.manager.Install(ctx, s.layout.Kernels, rel, emit)
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
	k, cerr := s.store.CreateKernel(ctx, model.Kernel{
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
