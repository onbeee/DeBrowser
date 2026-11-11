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
}

// NewInstanceService builds the service and reconciles persisted
// instance statuses (see the type comment). The database handle is
// owned by the caller (main) and is never closed here.
func NewInstanceService(db *database.DB, layout paths.Layout, log *slog.Logger) (*InstanceService, error) {
	s := &InstanceService{store: store.New(db), layout: layout, log: log}
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
// metadata. Safety rules, all enforced before anything is removed:
// the target must resolve strictly inside the layout's profiles
// directory (never the profiles root itself, never an escaping or
// absolute path), and a persisted "running" instance is refused.
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
	target, err := s.profileTarget(in.UserDataDir)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("删除 Profile 目录失败：%w", err)
	}
	if err := s.store.DeleteInstance(c, id); err != nil {
		return err
	}
	s.log.Info("instance and profile deleted", "id", id, "dir", in.UserDataDir)
	return nil
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
	// Final containment proof on the resolved absolute target.
	if _, err := s.profileTarget(slash); err != nil {
		return "", err
	}
	return slash, nil
}

// profileTarget resolves a stored (relative to data/) profile path
// to an absolute target and proves it is strictly inside the
// layout's profiles directory.
func (s *InstanceService) profileTarget(stored string) (string, error) {
	target := filepath.Join(s.layout.Data, filepath.FromSlash(stored))
	cleanTarget := filepath.Clean(target)
	cleanProfiles := filepath.Clean(s.layout.Profiles)
	if cleanTarget == cleanProfiles || !strings.HasPrefix(cleanTarget, cleanProfiles+string(os.PathSeparator)) {
		return "", fmt.Errorf("拒绝删除：目标路径不在 profiles 目录之内")
	}
	return cleanTarget, nil
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
