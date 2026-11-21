package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/pfiles"
	"github.com/onbeee/DeBrowser/internal/runner"
	"github.com/onbeee/DeBrowser/internal/store"
)

// InstanceService is the browser-instance management service exposed
// to the frontend over Wails bindings. It is a validating layer over
// internal/store: all SQL lives in the store, all filesystem effects
// are confined to the portable layout, and no method ever touches a
// path outside it.
//
// Status semantics (Stage 9B): model.Instance.Status is the
// PERSISTED lifecycle record, owned by the runtime manager in
// instance_runtime.go. It is written from verified process facts —
// a successful launch, an observed exit, a startup reconciliation
// that re-verified (or disproved) a recorded process — never merely
// assumed. Transient states (starting/stopping) live only in
// memory and are overlaid onto reads; a crash mid-transition
// therefore falls back to the last verified persisted state.
type InstanceService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger
	// launcher starts, inspects and re-attaches browser main
	// processes. A field so tests can substitute a fake.
	launcher runner.Launcher
	// ProxyPortFor, when set, resolves the loopback proxy port
	// bound to an instance (Stage 6C): the browser is then launched
	// with --proxy-server pointing at its own Mihomo listener.
	// Nil (or a 0 result) means the instance launches without a
	// proxy. It is a plain function so the instance service never
	// depends on the proxy service's type.
	ProxyPortFor func(instanceID string) int
	// Runtime tracking (Stage 9B), guarded by rtMu: procs holds
	// the verified live process of each running instance;
	// transient holds the in-flight starting/stopping state of an
	// instance whose persisted status has not caught up yet;
	// unverified holds the instances whose recorded process could
	// not be identity-verified at startup reconciliation — their
	// effective status is "unknown" (a safe hold) while their
	// persisted status stays "running" with the runtime record
	// kept, so the next startup reconciles them again.
	rtMu       sync.Mutex
	procs      map[string]*procWatch
	transient  map[string]model.InstanceStatus
	unverified map[string]bool
	// reconcileEvents accumulates the outcomes of the startup
	// runtime reconciliation (which runs in the constructor,
	// before the Auditor is wired) so ReportReconciliation can
	// flush them into the unified record once logging is ready.
	reconcileEvents []reconcileEvent
	// auditor, when set, receives one operation-audit record per
	// user-initiated mutation (Stage 5B).
	Auditor *logging.System
	// deleteRecord removes an instance row. It is a field so tests
	// can inject a failure into the middle of
	// DeleteInstanceAndProfile and verify the profile restore.
	deleteRecord func(ctx context.Context, id string) error
	// Test seams for deterministic race injection in
	// DeleteInstanceAndProfile (nil in production): afterVerifyHook
	// runs between path verification and the object-anchored
	// quarantine move; beforeRestoreHook runs between a failed
	// record deletion and the anchored restore;
	// wipeQuarantine overrides the anchored wipe.
	afterVerifyHook   func()
	beforeRestoreHook func()
	wipeQuarantine    func(q *pfiles.Quarantine) error
}

// NewInstanceService builds the service and reconciles persisted
// instance statuses against live process facts (see
// reconcileRuntimeAtStartup). The database handle is owned by the
// caller (main) and is never closed here.
func NewInstanceService(db *database.DB, layout paths.Layout, log *slog.Logger) (*InstanceService, error) {
	return newInstanceService(db, layout, log, runner.System())
}

func newInstanceService(db *database.DB, layout paths.Layout, log *slog.Logger, launcher runner.Launcher) (*InstanceService, error) {
	s := &InstanceService{
		store:      store.New(db),
		layout:     layout,
		log:        log,
		launcher:   launcher,
		procs:      map[string]*procWatch{},
		transient:  map[string]model.InstanceStatus{},
		unverified: map[string]bool{},
	}
	s.deleteRecord = s.store.DeleteInstance
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.reconcileRuntimeAtStartup(ctx); err != nil {
		return nil, fmt.Errorf("instance service: %w", err)
	}
	return s, nil
}

// Instance-file (profile directory) creation modes for
// CreateInstanceInput.ProfileMode (Stage 9B).
const (
	// ProfileModeDefault allocates the next sequential folder
	// name P-1, P-2, ... from the persistent profile counter and
	// creates the folder.
	ProfileModeDefault = "default"
	// ProfileModeCustom creates a NEW folder with the
	// user-supplied name in ProfileName.
	ProfileModeCustom = "custom"
	// ProfileModeExisting binds an existing, currently unbound
	// folder named by ProfileName without copying anything.
	ProfileModeExisting = "existing"
)

// Instance-file actions for UpdateInstanceInput.ProfileAction.
const (
	// ProfileActionNone leaves the bound instance file untouched.
	ProfileActionNone = ""
	// ProfileActionRename renames the bound folder itself to
	// ProfileName (a move, never a copy; data stays in place).
	ProfileActionRename = "rename"
	// ProfileActionSwap rebinds the instance to the existing
	// unbound folder named by ProfileName; the previously bound
	// folder stays on disk, unbound.
	ProfileActionSwap = "swap"
)

// CreateInstanceInput carries the user-editable fields for creation
// (Stage 9B: exactly four — name, kernel, proxy node, instance
// file). There is deliberately no proxy field: the proxy-node
// association stays part of the data model, but proxy management
// is not open yet, so instances are always created unbound
// (proxy_id NULL) and can run without one.
type CreateInstanceInput struct {
	Name string `json:"name"`
	// BrowserID associates the instance with one registered
	// browser (external_browsers ID) chosen in the form. It is
	// REQUIRED: an instance must name a registered kernel before
	// it can be created (and therefore started). The association
	// is by stable ID, never by name; the instance stores the
	// registration's ID and a display snapshot of its name, and
	// the executable path is always resolved live from the
	// registration.
	BrowserID string `json:"browserId"`
	// ProfileMode selects how the instance file is provided:
	// ProfileModeDefault, ProfileModeCustom or
	// ProfileModeExisting. Empty means ProfileModeDefault.
	ProfileMode string `json:"profileMode"`
	// ProfileName is the folder name for the custom and existing
	// modes (a single directory name directly under
	// data/profiles, never a path).
	ProfileName string `json:"profileName"`
}

// UpdateInstanceInput carries the editable fields of one instance
// (Stage 9B: name, kernel, proxy node — preserved, not editable —
// and the instance file). The stored proxy_id is not editable
// here: updates preserve whatever value the row already has.
type UpdateInstanceInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// BrowserID is the form's registered-browser choice (see
	// CreateInstanceInput). On an instance that already has an
	// association, an empty BrowserID clears it; on an instance
	// without one, the historical free-text kernel name/version
	// recorded before associations existed are preserved verbatim.
	// Changing the association is refused while the instance is
	// running (or its state is unconfirmed).
	BrowserID string `json:"browserId"`
	SortOrder int    `json:"sortOrder"`
	// ProfileAction optionally operates on the bound instance
	// file in the same save: ProfileActionRename renames the
	// bound folder to ProfileName; ProfileActionSwap rebinds the
	// instance to the existing unbound folder ProfileName. Both
	// are refused while the instance is running (or unconfirmed).
	ProfileAction string `json:"profileAction"`
	ProfileName   string `json:"profileName"`
}

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

// ListInstances returns all instances in display order.
func (s *InstanceService) ListInstances() ([]model.Instance, error) {
	c, cancel := ctx()
	defer cancel()
	list, err := s.store.ListInstances(c)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []model.Instance{}
	}
	s.resolveKernelPaths(c, list)
	s.applyEffectiveStatuses(list)
	return list, nil
}

// GetInstance returns one instance.
func (s *InstanceService) GetInstance(id string) (model.Instance, error) {
	c, cancel := ctx()
	defer cancel()
	in, err := s.store.GetInstance(c, id)
	if errors.Is(err, store.ErrNotFound) {
		return model.Instance{}, fmt.Errorf("实例不存在")
	}
	if err == nil {
		one := []model.Instance{in}
		s.resolveKernelPaths(c, one)
		s.applyEffectiveStatuses(one)
		in = one[0]
	}
	return in, err
}

// kernelAssociation resolves the form's registered-browser choice
// to the (kernel_id, kernel_name) pair stored on an instance: the
// registration's stable ID plus a display snapshot of its current
// name. An empty choice yields empty fields. A choice naming a
// registration that does not exist is an explicit error — never a
// silent free-text kernel record.
func (s *InstanceService) kernelAssociation(c context.Context, browserID string) (id, name string, err error) {
	browserID = strings.TrimSpace(browserID)
	if browserID == "" {
		return "", "", nil
	}
	b, err := s.store.GetExternalBrowser(c, browserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", "", fmt.Errorf("所选浏览器内核登记不存在")
		}
		return "", "", err
	}
	return b.ID, b.Name, nil
}

// resolveKernelPaths fills each instance's KernelExecutablePath
// from the registration its KernelID references — a live lookup, so
// re-selecting a registration's path is reflected on its instances
// immediately. A missing registration leaves the path empty.
func (s *InstanceService) resolveKernelPaths(c context.Context, list []model.Instance) {
	seen := map[string]string{}
	for i := range list {
		kid := list[i].KernelID
		if kid == "" {
			continue
		}
		if p, ok := seen[kid]; ok {
			list[i].KernelExecutablePath = p
			continue
		}
		p := ""
		if b, err := s.store.GetExternalBrowser(c, kid); err == nil {
			p = b.ExecutablePath
		}
		seen[kid] = p
		list[i].KernelExecutablePath = p
	}
}

// CreateInstance creates an instance (recorded, unified recording).
func (s *InstanceService) CreateInstance(operationID string, input CreateInstanceInput) (model.Instance, error) {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.create", "instance", "")
	inst, err := s.createInstance(input, op)
	if err != nil {
		finishOp(op, err)
		return inst, err
	}
	op.Succeed(map[string]string{"created_id": inst.ID})
	return inst, nil
}

func (s *InstanceService) createInstance(input CreateInstanceInput, op *logging.Operation) (model.Instance, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return model.Instance{}, fmt.Errorf("实例名称不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	if n, err := s.store.CountInstancesByName(c, name, ""); err != nil {
		return model.Instance{}, err
	} else if n > 0 {
		return model.Instance{}, fmt.Errorf("实例名称已存在，请使用其他名称")
	}
	// A kernel is required at creation time (Stage 9B): the
	// choice must name an existing registration.
	if strings.TrimSpace(input.BrowserID) == "" {
		return model.Instance{}, fmt.Errorf("请选择实例内核")
	}
	kernelID, kernelName, err := s.kernelAssociation(c, input.BrowserID)
	if err != nil {
		return model.Instance{}, err
	}
	// Provide the instance file according to the chosen mode.
	// createdDir is the folder THIS call created (default/custom);
	// it is removed again if the database insert fails, so a
	// failed creation never leaves an orphan folder behind. The
	// existing mode creates nothing and needs no rollback.
	var dirRel, createdDir string
	switch mode := strings.TrimSpace(input.ProfileMode); mode {
	case "", ProfileModeDefault:
		folder, abs, err := s.allocateDefaultProfile(c)
		if err != nil {
			return model.Instance{}, err
		}
		dirRel, createdDir = "profiles/"+folder, abs
		op.Step("分配默认实例文件", logging.StatusSuccess, map[string]string{"profile": folder})
	case ProfileModeCustom:
		folder := input.ProfileName
		if err := s.validateProfileName(folder); err != nil {
			return model.Instance{}, err
		}
		abs, err := s.createProfileDir(c, folder)
		if err != nil {
			return model.Instance{}, err
		}
		dirRel, createdDir = "profiles/"+folder, abs
		op.Step("创建自定义实例文件", logging.StatusSuccess, map[string]string{"profile": folder})
	case ProfileModeExisting:
		canonical, err := s.requireAvailableProfile(c, strings.TrimSpace(input.ProfileName))
		if err != nil {
			return model.Instance{}, err
		}
		dirRel = "profiles/" + canonical
		op.Step("选择已有实例文件", logging.StatusSuccess, map[string]string{"profile": canonical})
	default:
		return model.Instance{}, fmt.Errorf("实例文件方式无效")
	}
	id, err := store.NewID()
	if err != nil {
		s.rollbackCreatedDir(createdDir)
		return model.Instance{}, err
	}
	created, err := s.store.CreateInstance(c, model.Instance{
		ID: id, Name: name, UserDataDir: dirRel, // ProxyID stays nil: no proxy nodes exist to bind yet
		KernelID:   kernelID,
		KernelName: kernelName,
	})
	if err != nil {
		s.rollbackCreatedDir(createdDir)
		return model.Instance{}, mapConstraintError(err)
	}
	one := []model.Instance{created}
	s.resolveKernelPaths(c, one)
	created = one[0]
	s.log.Info("instance created", "id", created.ID, "name", created.Name, "dir", dirRel)
	return created, nil
}

// rollbackCreatedDir removes a profile folder a failed creation
// had just made. Only ever called with a folder this service
// created moments ago (empty by construction); removal uses
// os.Remove, which refuses a non-empty directory — a folder that
// somehow gained content is left in place rather than destroyed.
func (s *InstanceService) rollbackCreatedDir(abs string) {
	if abs == "" {
		return
	}
	if err := os.Remove(abs); err != nil {
		s.log.Warn("rollback of a newly created profile directory failed", "dir", abs, "error", err)
	}
}

// requireInstanceAtRest gates destructive or rebinding operations:
// the instance must be verifiably at rest. A running (or starting/
// stopping) instance refuses outright; an instance whose runtime
// state cannot be confirmed also refuses — acting on an
// unconfirmed browser could endanger its data.
func (s *InstanceService) requireInstanceAtRest(inst model.Instance, action string) error {
	switch s.effectiveStatus(inst) {
	case model.StatusStopped, model.StatusError:
		return nil
	case model.StatusUnknown:
		return fmt.Errorf("实例运行状态无法确认，不能%s", action)
	default:
		return fmt.Errorf("实例正在运行，不能%s", action)
	}
}

// UpdateInstance updates an instance (recorded, unified recording).
func (s *InstanceService) UpdateInstance(operationID string, input UpdateInstanceInput) (model.Instance, error) {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.update", "instance", input.ID)
	inst, err := s.updateInstance(input, op)
	finishOp(op, err)
	return inst, err
}

func (s *InstanceService) updateInstance(input UpdateInstanceInput, op *logging.Operation) (model.Instance, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return model.Instance{}, fmt.Errorf("实例名称不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	current, err := s.store.GetInstance(c, input.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return model.Instance{}, fmt.Errorf("实例不存在")
		}
		return model.Instance{}, err
	}
	if n, err := s.store.CountInstancesByName(c, name, input.ID); err != nil {
		return model.Instance{}, err
	} else if n > 0 {
		return model.Instance{}, fmt.Errorf("实例名称已存在，请使用其他名称")
	}

	// Work out what this save changes beyond the display name.
	kernelChange := strings.TrimSpace(input.BrowserID) != current.KernelID
	profileAction := strings.TrimSpace(input.ProfileAction)
	if profileAction != ProfileActionNone && profileAction != ProfileActionRename && profileAction != ProfileActionSwap {
		return model.Instance{}, fmt.Errorf("实例文件操作无效")
	}
	// Instance-file changes are refused unless the instance is
	// verifiably at rest: renaming or rebinding the folder under
	// a running browser — or one whose state cannot be
	// confirmed — could corrupt its data or strand its process.
	// Only operations that can endanger the profile folder are
	// gated this way.
	if profileAction != ProfileActionNone {
		switch s.effectiveStatus(current) {
		case model.StatusStopped, model.StatusError:
		case model.StatusUnknown:
			return model.Instance{}, fmt.Errorf("实例运行状态无法确认，不能更换实例文件")
		default:
			return model.Instance{}, fmt.Errorf("实例正在运行，不能更换实例文件")
		}
	}
	// A kernel change is metadata, like the display name: it
	// takes effect at the next launch and never acts on a
	// running process — the runtime record keeps the path the
	// browser was launched with, and no launch is possible
	// while the state is unconfirmed. It is refused only while
	// the instance is actually running or transitioning.
	if kernelChange {
		switch s.effectiveStatus(current) {
		case model.StatusRunning, model.StatusStarting, model.StatusStopping:
			return model.Instance{}, fmt.Errorf("实例正在运行，不能更换内核")
		}
	}

	updated := current
	updated.Name = name
	// updated.ProxyID and updated.StartURL are intentionally left
	// untouched: the stored proxy binding is data compatibility
	// (proxy management is not open yet), and the start URL is not
	// one of the four editable fields.
	switch {
	case strings.TrimSpace(input.BrowserID) != "":
		kernelID, kernelName, kerr := s.kernelAssociation(c, input.BrowserID)
		if kerr != nil {
			return model.Instance{}, kerr
		}
		updated.KernelID = kernelID
		updated.KernelName = kernelName
		updated.KernelVersion = ""
	case current.KernelID != "":
		// The form's empty choice explicitly clears an existing
		// association.
		updated.KernelID = ""
		updated.KernelName = ""
		updated.KernelVersion = ""
	default:
		// No association and none chosen: historical free-text
		// kernel name/version recorded before associations existed
		// stay untouched — an unrelated edit never drops them.
	}
	updated.SortOrder = input.SortOrder

	// Instance-file operations run BEFORE the database write; a
	// failed write rolls the filesystem back, so the record and
	// the folder can never disagree about the binding.
	var rollback func() error
	switch profileAction {
	case ProfileActionRename:
		newDir, rb, rerr := s.renameProfileDir(c, current, input.ProfileName)
		if rerr != nil {
			return model.Instance{}, rerr
		}
		updated.UserDataDir = newDir
		rollback = rb
		op.Step("重命名实例文件", logging.StatusSuccess, map[string]string{
			"from": profileFolder(current.UserDataDir), "to": profileFolder(newDir),
		})
	case ProfileActionSwap:
		canonical, serr := s.requireAvailableProfile(c, strings.TrimSpace(input.ProfileName))
		if serr != nil {
			return model.Instance{}, serr
		}
		updated.UserDataDir = "profiles/" + canonical
		op.Step("更换实例文件", logging.StatusSuccess, map[string]string{
			"from": profileFolder(current.UserDataDir), "to": canonical,
		})
	}

	saved, err := s.store.UpdateInstance(c, updated)
	if err != nil {
		if rollback != nil {
			if rerr := rollback(); rerr != nil {
				// The folder moved but the record could not
				// follow, and the move-back failed too: surface
				// the inconsistency loudly instead of hiding it.
				_ = s.store.SetInstanceStatus(c, current.ID, model.StatusError)
				return model.Instance{}, fmt.Errorf("保存实例失败，且实例文件恢复失败（记录与文件夹可能不一致，请勿启动该实例）：%w", err)
			}
		}
		return model.Instance{}, mapConstraintError(err)
	}
	one := []model.Instance{saved}
	s.resolveKernelPaths(c, one)
	s.applyEffectiveStatuses(one)
	return one[0], nil
}

// renameProfileDir renames the instance's bound folder to newName
// and returns the new stored user_data_dir plus a rollback that
// undoes the filesystem move. Rules: the target must be a valid
// folder name, free (not bound, not on disk — Windows
// case-insensitively), and the move itself is a plain rename of
// the verified folder inside data/profiles: no copy, no merge, no
// data loss. When the bound folder does not exist on disk yet
// (the instance was never started), an empty folder is created at
// the new name instead — there is no data to move.
func (s *InstanceService) renameProfileDir(c context.Context, inst model.Instance, newName string) (string, func() error, error) {
	if err := s.validateProfileName(newName); err != nil {
		return "", nil, err
	}
	oldFolder := profileFolder(inst.UserDataDir)
	if oldFolder == "" {
		return "", nil, fmt.Errorf("实例文件路径异常，无法重命名")
	}
	if foldName(oldFolder) == foldName(newName) && oldFolder != newName {
		// A case-only rename is a real rename on Windows too;
		// fall through and let the OS do it.
	} else if oldFolder == newName {
		return inst.UserDataDir, func() error { return nil }, nil
	}
	taken, err := s.profileFolderTaken(c, newName, foldName(oldFolder))
	if err != nil {
		return "", nil, err
	}
	if taken {
		return "", nil, fmt.Errorf("目标实例文件名称 %q 已存在，无法重命名", newName)
	}
	src := filepath.Join(s.layout.Profiles, oldFolder)
	dst := filepath.Join(s.layout.Profiles, newName)
	_, lerr := os.Lstat(src)
	switch {
	case lerr == nil:
		// The folder exists: it must be a real directory inside
		// the managed root, never a link out of it.
		if _, verr := s.verifiedProfileTarget(inst.UserDataDir); verr != nil {
			return "", nil, fmt.Errorf("实例文件无法安全访问：%w", verr)
		}
		if err := os.Rename(src, dst); err != nil {
			return "", nil, fmt.Errorf("重命名实例文件失败：%w", err)
		}
		return "profiles/" + newName, func() error { return os.Rename(dst, src) }, nil
	case errors.Is(lerr, os.ErrNotExist):
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return "", nil, fmt.Errorf("重命名实例文件失败：%w", err)
		}
		return "profiles/" + newName, func() error { return os.Remove(dst) }, nil
	default:
		return "", nil, fmt.Errorf("实例文件无法访问：%w", lerr)
	}
}

// DeleteInstance removes ONLY the instance's metadata (and, via the
// schema cascade, its bookmarks). The profile directory on disk is
// deliberately left untouched: browser data is never deleted as a
// side effect of removing a record.
// DeleteInstance deletes instance metadata only (recorded,
// unified recording).
func (s *InstanceService) DeleteInstance(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.delete", "instance", id)
	err := s.deleteInstance(id)
	finishOp(op, err)
	return err
}

func (s *InstanceService) deleteInstance(id string) error {
	c, cancel := ctx()
	defer cancel()
	inst, err := s.store.GetInstance(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	if err := s.requireInstanceAtRest(inst, "删除"); err != nil {
		return err
	}
	if err := s.store.DeleteInstance(c, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	s.log.Info("instance metadata deleted (profile files kept)", "id", id)
	return nil
}

// DeleteInstanceAndProfile is the explicit, destructive counterpart
// of DeleteInstance: it removes the profile directory AND the
// metadata. It is structured so no failure loses data silently:
//
//  1. The stored path is proven safe (verifiedProfileTarget: the
//     directory itself is not a link, and its fully resolved real
//     path lies strictly inside the profiles directory).
//  2. internal/pfiles moves the VERIFIED OBJECT — not the path
//     string — into a quarantine slot under profiles: Linux anchors
//     the move to directory fds (openat O_NOFOLLOW descent +
//     renameat2 NOREPLACE between pinned fds, with a post-move
//     identity check), Windows renames through the target's own
//     directory handle after verifying the handle's identity.
//     Swapping any path component after step 1 therefore cannot
//     redirect the move to an outside directory; anything that
//     cannot be anchored is refused with data and record intact.
//  3. The database record is deleted. If that fails, the object is
//     moved back through the same anchor — the operation aborts
//     with the profile and the record both intact.
//  4. Only with the record gone is the quarantine tree wiped by
//     anchored deletion (internal/safedelete). A wipe failure
//     leaves the data in the quarantine slot — reported in the
//     error, never silently lost.
//
// An instance that is running (or whose runtime state cannot
// be confirmed) is refused before any of this.
// DeleteInstanceAndProfile deletes an instance and its profile
// directory (recorded with per-phase steps, unified recording).
func (s *InstanceService) DeleteInstanceAndProfile(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "浏览器实例", "instance.delete_with_profile", "instance", id)
	err := s.deleteInstanceAndProfile(id, op)
	finishOp(op, err)
	return err
}

func (s *InstanceService) deleteInstanceAndProfile(id string, op *logging.Operation) error {
	c, cancel := ctx()
	defer cancel()
	in, err := s.store.GetInstance(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	if err := s.requireInstanceAtRest(in, "删除"); err != nil {
		return err
	}
	target, err := s.verifiedProfileTarget(in.UserDataDir)
	if err != nil {
		op.Step("校验 Profile 路径", logging.StatusFailed, nil)
		return err
	}
	op.Step("校验 Profile 路径", logging.StatusSuccess, nil)
	if target == "" {
		// Nothing on disk: only the record needs to go.
		if err := s.store.DeleteInstance(c, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("实例不存在")
			}
			return err
		}
		op.Step("删除数据库记录", logging.StatusSuccess, nil)
		s.log.Info("instance deleted (no profile directory on disk)", "id", id)
		return nil
	}
	if s.afterVerifyHook != nil {
		s.afterVerifyHook()
	}

	q, err := s.moveToQuarantine(target)
	if err != nil {
		op.Step("移动 Profile 到隔离区", logging.StatusFailed, nil)
		return err
	}
	op.Step("移动 Profile 到隔离区", logging.StatusSuccess, nil)
	defer func() { _ = q.Close() }()

	if err := s.deleteRecord(c, id); err != nil {
		op.Step("删除数据库记录", logging.StatusFailed, nil)
		// Restore the object to its original location through the
		// same anchor; the operation must leave the world exactly
		// as it found it.
		if s.beforeRestoreHook != nil {
			s.beforeRestoreHook()
		}
		if rerr := q.Restore(); rerr != nil {
			return fmt.Errorf("删除实例记录失败，且 Profile 目录恢复失败（数据保留在隔离目录 %s）：%w", q.Path(), err)
		}
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	op.Step("删除数据库记录", logging.StatusSuccess, nil)
	wipe := s.wipeQuarantine
	if wipe == nil {
		wipe = func(q *pfiles.Quarantine) error { return q.Wipe() }
	}
	if err := wipe(q); err != nil {
		op.Step("安全擦除 Profile 数据", logging.StatusFailed, nil)
		return fmt.Errorf("实例记录已删除，但 Profile 数据清理不完全（残留在隔离目录 %s）：%w", q.Path(), err)
	}
	op.Step("安全擦除 Profile 数据", logging.StatusSuccess, nil)
	s.log.Info("instance and profile deleted", "id", id, "dir", in.UserDataDir)
	return nil
}

// moveToQuarantine moves target into a fresh quarantine slot,
// retrying with another slot name when a name turns out to be
// occupied (the platform move never overwrites an occupant).
func (s *InstanceService) moveToQuarantine(target string) (*pfiles.Quarantine, error) {
	for attempt := 0; attempt < 3; attempt++ {
		id, err := store.NewID()
		if err != nil {
			return nil, err
		}
		q, err := pfiles.MoveToQuarantine(s.layout.Profiles, target, ".debrowser-quarantine-"+id)
		if err == nil {
			return q, nil
		}
		if errors.Is(err, pfiles.ErrOccupied) {
			continue
		}
		if errors.Is(err, pfiles.ErrUnsafe) {
			return nil, fmt.Errorf("无法证明 Profile 目录可被安全隔离，已拒绝删除：%w", err)
		}
		return nil, fmt.Errorf("隔离 Profile 目录失败：%w", err)
	}
	return nil, fmt.Errorf("无法分配隔离目录")
}

// validateProfileComponent checks one profile directory name
// component against the Windows reinterpretation rules: NTFS
// alternate data streams (':'), trailing dot/space aliasing,
// reserved device names, and smuggled separators are all refused
// up front rather than discovered later.
func validateProfileComponent(comp string) error {
	if comp == "" || comp == "." || comp == ".." {
		return fmt.Errorf("数据目录包含无效的路径组件")
	}
	if strings.ContainsAny(comp, "<>:\"|?*\\") {
		return fmt.Errorf("数据目录组件 %q 包含不允许的字符", comp)
	}
	for _, r := range comp {
		if r < 0x20 {
			return fmt.Errorf("数据目录组件 %q 包含控制字符", comp)
		}
	}
	if strings.HasSuffix(comp, ".") || strings.HasSuffix(comp, " ") {
		return fmt.Errorf("数据目录组件 %q 不能以点或空格结尾", comp)
	}
	base := comp
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9",
		// Microsoft's naming rules also reserve the superscript
		// forms: Windows treats the ISO/IEC 8859-1 superscript
		// digits ¹ ² ³ as digits in COM#/LPT# device names.
		"COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return fmt.Errorf("数据目录组件 %q 是 Windows 保留设备名", comp)
	}
	return nil
}

// verifiedProfileTarget proves that the stored profile path is safe
// to delete and returns the RESOLVED absolute path to remove (""
// when nothing exists on disk). String containment is only the first
// gate; the real proof is filesystem-based:
//
//  1. The lexical target must be strictly inside the profiles dir.
//  2. The target itself must not be a symbolic link / reparse point
//     (deleting "through" it is refused outright, wherever it aims).
//  3. resolveFinalPath resolves EVERY component — intermediate
//     symlinks and Windows junctions/reparse points included
//     (EvalSymlinks on Unix, GetFinalPathNameByHandle on Windows,
//     where EvalSymlinks alone misses a junction that is the final
//     component) — and the resolved target must still be strictly
//     inside the resolved profiles directory. Anything that cannot
//     be proven inside is refused.
//
// Deleting the resolved path (rather than the lexical one) keeps the
// verified location and the deleted location identical.
func (s *InstanceService) verifiedProfileTarget(stored string) (string, error) {
	target := filepath.Join(s.layout.Data, filepath.FromSlash(stored))
	cleanTarget := filepath.Clean(target)
	cleanProfiles := filepath.Clean(s.layout.Profiles)
	if cleanTarget == cleanProfiles || !strings.HasPrefix(cleanTarget, cleanProfiles+string(os.PathSeparator)) {
		return "", fmt.Errorf("拒绝删除：目标路径不在 profiles 目录之内")
	}
	info, err := os.Lstat(cleanTarget)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil // nothing on disk: nothing to protect
		}
		return "", fmt.Errorf("拒绝删除：无法检查目标路径：%w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("拒绝删除：Profile 目录本身是符号链接")
	}
	resolvedProfiles, err := resolveFinalPath(cleanProfiles)
	if err != nil {
		return "", fmt.Errorf("拒绝删除：无法解析 profiles 目录：%w", err)
	}
	resolvedTarget, err := resolveFinalPath(cleanTarget)
	if err != nil {
		return "", fmt.Errorf("拒绝删除：无法解析目标真实路径：%w", err)
	}
	if resolvedTarget == resolvedProfiles || !strings.HasPrefix(resolvedTarget, resolvedProfiles+string(os.PathSeparator)) {
		return "", fmt.Errorf("拒绝删除：目标真实路径不在 profiles 目录之内")
	}
	return resolvedTarget, nil
}

// mapConstraintError turns SQLite constraint failures into messages
// the UI can show directly.
func mapConstraintError(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: browser_instances.name"):
		return fmt.Errorf("实例名称已存在，请换一个名称")
	case strings.Contains(msg, "UNIQUE constraint failed: browser_instances.user_data_dir"):
		return fmt.Errorf("该数据目录已被其他实例占用")
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("关联数据不存在或已被删除")
	default:
		return err
	}
}
