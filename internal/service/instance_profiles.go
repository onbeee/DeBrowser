package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Instance files (user-facing name for profile directories) live
// as single folders directly under data/profiles. This file owns
// their naming, allocation, scanning and selection rules:
//
//   - one instance binds exactly one folder, one folder binds at
//     most one instance (the database unique index on
//     user_data_dir backs the service checks);
//   - default names come from the persistent profile counter and
//     are never reused, even after deletions;
//   - only folders that provably exist, are directories, are not
//     bound, are not referenced by a runtime record and are not
//     internal (dot-prefixed) are ever offered for selection —
//     anything in an unknown state stays hidden, never guessed
//     usable;
//   - scanning reads directory NAMES only, never the browser data
//     inside a folder.

// validateProfileName checks one instance-file folder name: a
// single path component, safe under the Windows reinterpretation
// rules, and not in the internal dot-prefixed namespace the app
// itself uses (quarantine slots).
func (s *InstanceService) validateProfileName(name string) error {
	if name == "" {
		return fmt.Errorf("实例文件名称不能为空")
	}
	if strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("实例文件名称不能包含路径分隔符")
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("实例文件名称不能以点开头")
	}
	return validateProfileComponent(name)
}

// profileFolder extracts the folder name from a stored
// user_data_dir ("profiles/<name>"). It returns "" when the stored
// form is not a direct child of profiles/ (legacy or damaged
// data): such an instance has no manageable instance file.
func profileFolder(stored string) string {
	rest, ok := strings.CutPrefix(filepath.ToSlash(stored), "profiles/")
	if !ok || rest == "" || strings.Contains(rest, "/") {
		return ""
	}
	return rest
}

// foldName normalises a folder name for the case-insensitive
// comparisons Windows filesystem semantics require.
func foldName(name string) string { return strings.ToLower(name) }

// boundProfileFolders returns the folded names of every folder
// currently bound to an instance.
func (s *InstanceService) boundProfileFolders(c context.Context) (map[string]bool, error) {
	dirs, err := s.store.ListInstanceUserDataDirs(c)
	if err != nil {
		return nil, err
	}
	bound := map[string]bool{}
	for _, d := range dirs {
		if f := profileFolder(d); f != "" {
			bound[foldName(f)] = true
		}
	}
	return bound, nil
}

// diskProfileFolders lists the folders physically present directly
// under data/profiles, keyed by folded name with the on-disk
// spelling as value. Only real directories count: a symlink or a
// file is an unknown state, never a selectable instance file.
// Internal dot-prefixed entries (quarantine slots) are excluded.
func (s *InstanceService) diskProfileFolders() (map[string]string, error) {
	entries, err := os.ReadDir(s.layout.Profiles)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("读取实例文件目录失败：%w", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		out[foldName(name)] = name
	}
	return out, nil
}

// profileFolderTaken reports whether a folder name is unavailable
// for creation or renaming: bound to an instance, or already
// present on disk (case-insensitively). exceptFold exempts one
// folded name (the instance's own current folder, for renames).
func (s *InstanceService) profileFolderTaken(c context.Context, name, exceptFold string) (bool, error) {
	if foldName(name) != exceptFold {
		bound, err := s.boundProfileFolders(c)
		if err != nil {
			return false, err
		}
		if bound[foldName(name)] {
			return true, nil
		}
		disk, err := s.diskProfileFolders()
		if err != nil {
			return false, err
		}
		if _, ok := disk[foldName(name)]; ok {
			return true, nil
		}
	}
	return false, nil
}

// createProfileDir creates a fresh instance-file folder after
// proving the name free. The leaf is created with os.Mkdir (not
// MkdirAll) so an occupant appearing after the check is an error,
// never an overwrite.
func (s *InstanceService) createProfileDir(c context.Context, name string) (string, error) {
	taken, err := s.profileFolderTaken(c, name, "")
	if err != nil {
		return "", err
	}
	if taken {
		return "", fmt.Errorf("实例文件名称 %q 已存在，请使用其他名称", name)
	}
	if err := os.MkdirAll(s.layout.Profiles, 0o755); err != nil {
		return "", fmt.Errorf("创建实例文件目录失败：%w", err)
	}
	abs := filepath.Join(s.layout.Profiles, name)
	if err := os.Mkdir(abs, 0o755); err != nil {
		if os.IsExist(err) {
			return "", fmt.Errorf("实例文件名称 %q 已存在，请使用其他名称", name)
		}
		return "", fmt.Errorf("创建实例文件目录失败：%w", err)
	}
	return abs, nil
}

// allocateDefaultProfile consumes sequence numbers from the
// persistent counter until one yields a folder name that is
// neither bound nor present on disk, creates the folder, and
// returns its name and absolute path. Consumed numbers are never
// handed out again — the counter only moves forward, so a skipped
// or later-deleted number stays retired.
func (s *InstanceService) allocateDefaultProfile(c context.Context) (string, string, error) {
	for attempt := 0; attempt < 10000; attempt++ {
		seq, err := s.store.NextProfileSeq(c)
		if err != nil {
			return "", "", err
		}
		name := fmt.Sprintf("P-%d", seq)
		taken, err := s.profileFolderTaken(c, name, "")
		if err != nil {
			return "", "", err
		}
		if taken {
			continue
		}
		abs, err := s.createProfileDir(c, name)
		if err != nil {
			return "", "", err
		}
		return name, abs, nil
	}
	return "", "", fmt.Errorf("无法分配默认实例文件编号")
}

// availableProfiles computes the folders safe to offer for
// selection: present on disk as real directories, not bound to any
// instance, not referenced by any runtime record (a folder a
// possibly-running browser may be using is never offered), and
// carrying a valid folder name.
func (s *InstanceService) availableProfiles(c context.Context) ([]model.ProfileDirInfo, error) {
	bound, err := s.boundProfileFolders(c)
	if err != nil {
		return nil, err
	}
	runtimes, err := s.store.ListInstanceRuntimes(c)
	if err != nil {
		return nil, err
	}
	inUse := map[string]bool{}
	for _, rt := range runtimes {
		if f := profileFolder(rt.UserDataDir); f != "" {
			inUse[foldName(f)] = true
		}
	}
	disk, err := s.diskProfileFolders()
	if err != nil {
		return nil, err
	}
	out := []model.ProfileDirInfo{}
	for folded, name := range disk {
		if bound[folded] || inUse[folded] {
			continue
		}
		if err := s.validateProfileName(name); err != nil {
			continue // an unnameable folder is an unknown state: hidden
		}
		out = append(out, model.ProfileDirInfo{
			Name: name,
			Path: filepath.Join(s.layout.Profiles, name),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListAvailableProfiles returns the instance files that may be
// bound to a new or edited instance right now (see
// availableProfiles for the safety rules).
func (s *InstanceService) ListAvailableProfiles() ([]model.ProfileDirInfo, error) {
	c, cancel := ctx()
	defer cancel()
	return s.availableProfiles(c)
}

// requireAvailableProfile proves that name is currently a
// selectable instance file and returns its canonical on-disk
// spelling. It re-checks at operation time: a folder that was
// selectable when a form opened may have been bound since.
func (s *InstanceService) requireAvailableProfile(c context.Context, name string) (string, error) {
	if err := s.validateProfileName(name); err != nil {
		return "", err
	}
	avail, err := s.availableProfiles(c)
	if err != nil {
		return "", err
	}
	for _, p := range avail {
		if foldName(p.Name) == foldName(name) {
			return p.Name, nil
		}
	}
	return "", fmt.Errorf("所选实例文件不存在或已被其他实例使用")
}
