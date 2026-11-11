package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/safedelete"
	"github.com/onbeee/DeBrowser/internal/store"
)

// InstanceService is the browser-instance management service exposed
// to the frontend over Wails bindings (Stage 3). It is a thin,
// validating layer over internal/store: all SQL lives in the store,
// all filesystem effects are confined to the portable layout, and no
// method ever touches a path outside it.
//
// Status semantics: model.Instance.Status is the PERSISTED lifecycle
// record. Stage 3 has no browser process manager, so no instance can
// actually be running: at construction, any row still marked
// "running" (a leftover from a crashed or older process) is
// reconciled to "stopped". Later stages will own live transitions.
type InstanceService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger
	// deleteRecord removes an instance row. It is a field so tests
	// can inject a failure into the middle of
	// DeleteInstanceAndProfile and verify the profile restore.
	deleteRecord func(ctx context.Context, id string) error
}

// NewInstanceService builds the service and reconciles persisted
// instance statuses (see the type comment). The database handle is
// owned by the caller (main) and is never closed here.
func NewInstanceService(db *database.DB, layout paths.Layout, log *slog.Logger) (*InstanceService, error) {
	s := &InstanceService{store: store.New(db), layout: layout, log: log}
	s.deleteRecord = s.store.DeleteInstance
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.store.ReconcileInstanceStatuses(ctx)
	if err != nil {
		return nil, fmt.Errorf("instance service: %w", err)
	}
	if n > 0 {
		log.Info("reconciled stale running instances to stopped", "count", n)
	}
	return s, nil
}

// CreateInstanceInput carries the user-editable fields for creation.
// UserDataDir is optional: empty means the default profiles/<id>.
type CreateInstanceInput struct {
	Name          string  `json:"name"`
	ProxyID       *string `json:"proxyId"`
	StartURL      string  `json:"startUrl"`
	KernelName    string  `json:"kernelName"`
	KernelVersion string  `json:"kernelVersion"`
	UserDataDir   string  `json:"userDataDir"`
}

// UpdateInstanceInput carries the editable fields of one instance.
// The profile directory of an existing instance cannot be changed
// here (moving live profile data is a separate, explicit operation).
type UpdateInstanceInput struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	ProxyID       *string `json:"proxyId"`
	StartURL      string  `json:"startUrl"`
	KernelName    string  `json:"kernelName"`
	KernelVersion string  `json:"kernelVersion"`
	SortOrder     int     `json:"sortOrder"`
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
	return in, err
}

// ListProxies returns all proxy nodes (read-only; proxy management
// itself is Stage 4) so the instance form can offer assignment.
func (s *InstanceService) ListProxies() ([]model.Proxy, error) {
	c, cancel := ctx()
	defer cancel()
	list, err := s.store.ListProxies(c)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []model.Proxy{}
	}
	return list, nil
}

// CreateInstance validates and creates an instance. The default
// profile directory is profiles/<id>; an explicit directory must be
// a relative path inside profiles/.
func (s *InstanceService) CreateInstance(input CreateInstanceInput) (model.Instance, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return model.Instance{}, fmt.Errorf("实例名称不能为空")
	}
	id, err := store.NewID()
	if err != nil {
		return model.Instance{}, err
	}
	dir := strings.TrimSpace(input.UserDataDir)
	if dir == "" {
		dir = "profiles/" + id
	} else {
		cleaned, err := s.validateProfileDir(dir)
		if err != nil {
			return model.Instance{}, err
		}
		dir = cleaned
	}
	c, cancel := ctx()
	defer cancel()
	if input.ProxyID != nil {
		if _, err := s.store.GetProxy(c, *input.ProxyID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return model.Instance{}, fmt.Errorf("所选代理不存在")
			}
			return model.Instance{}, err
		}
	}
	created, err := s.store.CreateInstance(c, model.Instance{
		ID: id, Name: name, UserDataDir: dir, ProxyID: input.ProxyID,
		StartURL:      strings.TrimSpace(input.StartURL),
		KernelName:    strings.TrimSpace(input.KernelName),
		KernelVersion: strings.TrimSpace(input.KernelVersion),
	})
	if err != nil {
		return model.Instance{}, mapConstraintError(err)
	}
	s.log.Info("instance created", "id", created.ID, "name", created.Name)
	return created, nil
}

// UpdateInstance validates and updates an instance's editable fields.
func (s *InstanceService) UpdateInstance(input UpdateInstanceInput) (model.Instance, error) {
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
	if input.ProxyID != nil {
		if _, err := s.store.GetProxy(c, *input.ProxyID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return model.Instance{}, fmt.Errorf("所选代理不存在")
			}
			return model.Instance{}, err
		}
	}
	current.Name = name
	current.ProxyID = input.ProxyID
	current.StartURL = strings.TrimSpace(input.StartURL)
	current.KernelName = strings.TrimSpace(input.KernelName)
	current.KernelVersion = strings.TrimSpace(input.KernelVersion)
	current.SortOrder = input.SortOrder
	updated, err := s.store.UpdateInstance(c, current)
	if err != nil {
		return model.Instance{}, mapConstraintError(err)
	}
	return updated, nil
}

// DeleteInstance removes ONLY the instance's metadata (and, via the
// schema cascade, its bookmarks). The profile directory on disk is
// deliberately left untouched: browser data is never deleted as a
// side effect of removing a record.
func (s *InstanceService) DeleteInstance(id string) error {
	c, cancel := ctx()
	defer cancel()
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
//  2. The directory is RENAMED into a quarantine slot directly
//     under profiles (same volume, atomic). Renaming a directory
//     never follows it; from here on the data sits at a path this
//     process alone chose, so the check/use gap is closed: whatever
//     the anchored wipe later removes can only be what the rename
//     actually moved.
//  3. The database record is deleted. If that fails, the quarantine
//     directory is renamed BACK — the operation aborts with the
//     profile and the record both intact.
//  4. Only with the record gone is the quarantine tree wiped, by
//     internal/safedelete's anchored traversal (never follows links
//     out of the tree). A wipe failure leaves the data in the
//     quarantine slot — reported in the error, never silently lost.
//
// A persisted "running" instance is refused before any of this.
func (s *InstanceService) DeleteInstanceAndProfile(id string) error {
	c, cancel := ctx()
	defer cancel()
	in, err := s.store.GetInstance(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	if in.Status == model.StatusRunning {
		return fmt.Errorf("实例正在运行，不能删除其 Profile 数据")
	}
	target, err := s.verifiedProfileTarget(in.UserDataDir)
	if err != nil {
		return err
	}
	if target == "" {
		// Nothing on disk: only the record needs to go.
		if err := s.store.DeleteInstance(c, id); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("实例不存在")
			}
			return err
		}
		s.log.Info("instance deleted (no profile directory on disk)", "id", id)
		return nil
	}

	quarantine, err := s.quarantinePath()
	if err != nil {
		return err
	}
	if err := os.Rename(target, quarantine); err != nil {
		return fmt.Errorf("隔离 Profile 目录失败：%w", err)
	}
	if err := s.deleteRecord(c, id); err != nil {
		// Restore the directory to its original location; the
		// operation must leave the world exactly as it found it.
		if rerr := os.Rename(quarantine, target); rerr != nil {
			return fmt.Errorf("删除实例记录失败，且 Profile 目录恢复失败（数据保留在隔离目录 %s）：%w", quarantine, err)
		}
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("实例不存在")
		}
		return err
	}
	if err := safedelete.RemoveTree(s.layout.Profiles, quarantine); err != nil {
		return fmt.Errorf("实例记录已删除，但 Profile 数据清理不完全（残留在隔离目录 %s）：%w", quarantine, err)
	}
	s.log.Info("instance and profile deleted", "id", id, "dir", in.UserDataDir)
	return nil
}

// quarantinePath picks a fresh, unused quarantine slot directly
// under the profiles directory.
func (s *InstanceService) quarantinePath() (string, error) {
	for attempt := 0; attempt < 3; attempt++ {
		id, err := store.NewID()
		if err != nil {
			return "", err
		}
		candidate := filepath.Join(s.layout.Profiles, ".debrowser-quarantine-"+id)
		if _, err := os.Lstat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("无法分配隔离目录")
}

// validateProfileDir checks a user-supplied profile directory: it
// must be relative, and its cleaned form must live under profiles/.
// It returns the cleaned, slash-separated stored form.
func (s *InstanceService) validateProfileDir(dir string) (string, error) {
	if filepath.IsAbs(dir) || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, "\\") {
		return "", fmt.Errorf("数据目录必须是相对路径")
	}
	clean := filepath.Clean(dir)
	slash := filepath.ToSlash(clean)
	if slash == "profiles" || !strings.HasPrefix(slash, "profiles/") {
		return "", fmt.Errorf("数据目录必须位于 profiles 目录之下")
	}
	if strings.Contains(slash, "../") || slash == ".." {
		return "", fmt.Errorf("数据目录不能超出 profiles 目录")
	}
	// If the directory (or a path to it) already exists, it must also
	// pass the same real-path verification used for deletion: a
	// pre-planted symlink inside profiles must not legitimise an
	// outside location as a profile directory.
	full := filepath.Join(s.layout.Data, clean)
	if _, err := os.Lstat(full); err == nil {
		if _, err := s.verifiedProfileTarget(slash); err != nil {
			return "", err
		}
	}
	return slash, nil
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
