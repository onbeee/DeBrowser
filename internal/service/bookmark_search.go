package service

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/chromebookmarks"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Aggregated cross-instance search (Stage 9C sync edit/delete).
//
// A search only reads instances that are confirmed stopped and
// readable. Results are aggregated by content: the frontend only
// ever sees a group's name, URL and the number of distinct
// instances holding it — never instance names or per-instance
// detail. The exact targets (instance, root, node id, parent path
// and a name/URL snapshot) stay server-side in a short-lived
// session; applying an edit or delete re-checks the instance state,
// re-reads the file and re-locates every target node by id,
// verifying it still matches the snapshot before touching it.

type bookmarkTarget struct {
	InstanceID string
	RootKey    string
	NodeID     string
	ParentID   string
	Path       []string
	Name       string
	URL        string
}

type bookmarkGroup struct {
	Key     string
	Name    string
	URL     string
	Targets []bookmarkTarget
}

type bookmarkSearchSession struct {
	Kind      string // model.BookmarkKindBookmark or model.BookmarkKindFolder
	Groups    []bookmarkGroup
	CreatedAt time.Time
}

type BookmarkSearchGroup struct {
	Key           string `json:"key"`
	Name          string `json:"name"`
	URL           string `json:"url"`
	InstanceCount int    `json:"instanceCount"`
}

type BookmarkSearchResult struct {
	Token              string                `json:"token"`
	Kind               string                `json:"kind"`
	Groups             []BookmarkSearchGroup `json:"groups"`
	UncheckedInstances int                   `json:"uncheckedInstances"`
}

const (
	searchSessionTTL = 30 * time.Minute
	maxSearchSession = 8
)

// SearchInstanceBookmarks aggregates matching bookmarks or folders
// across all readable, confirmed-stopped instances.
func (s *BookmarkService) SearchInstanceBookmarks(operationID string, query string, kind string) (BookmarkSearchResult, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.search", "browser_instance", "")
	result, err := s.searchInstanceBookmarks(query, kind)
	finishOp(op, err)
	return result, err
}

func (s *BookmarkService) searchInstanceBookmarks(query string, kind string) (BookmarkSearchResult, error) {
	result := BookmarkSearchResult{Kind: kind, Groups: []BookmarkSearchGroup{}}
	if kind != model.BookmarkKindBookmark && kind != model.BookmarkKindFolder {
		return result, fmt.Errorf("搜索类型无效")
	}
	q := strings.ToLower(strings.TrimSpace(query))
	c, cancel := ctx()
	defer cancel()
	instances, err := s.store.ListInstances(c)
	if err != nil {
		return result, err
	}
	groups := map[string]*bookmarkGroup{}
	var order []string
	addTarget := func(key string, t bookmarkTarget) {
		g, ok := groups[key]
		if !ok {
			g = &bookmarkGroup{Key: key, Name: t.Name, URL: t.URL}
			groups[key] = g
			order = append(order, key)
		}
		g.Targets = append(g.Targets, t)
	}
	for _, inst := range instances {
		_, dir, gate, gerr := s.instances.InstanceBookmarkAccess(inst.ID)
		if gerr != nil || gate != BookmarkGateStopped {
			result.UncheckedInstances++
			continue
		}
		unlock := chromebookmarks.LockProfile(dir)
		loaded, lerr := chromebookmarks.Load(dir)
		if lerr != nil {
			result.UncheckedInstances++
			unlock()
			continue
		}
		if kind == model.BookmarkKindBookmark {
			loaded.WalkBookmarks(func(b chromebookmarks.FlatBookmark) {
				if q != "" && !strings.Contains(strings.ToLower(b.Name), q) && !strings.Contains(strings.ToLower(b.URL), q) {
					return
				}
				addTarget("b\x00"+b.Name+"\x00"+b.URL, bookmarkTarget{
					InstanceID: inst.ID, RootKey: b.RootKey, NodeID: b.NodeID,
					ParentID: b.ParentID, Path: b.Path, Name: b.Name, URL: b.URL,
				})
			})
		} else {
			loaded.WalkFolders(func(fl chromebookmarks.FlatFolder) {
				if q != "" && !strings.Contains(strings.ToLower(fl.Name), q) {
					return
				}
				key := "f\x00" + fl.RootKey + "\x00" + strings.Join(fl.Path, "\x00") + "\x00" + fl.Name
				addTarget(key, bookmarkTarget{
					InstanceID: inst.ID, RootKey: fl.RootKey, NodeID: fl.NodeID,
					ParentID: fl.ParentID, Path: fl.Path, Name: fl.Name,
				})
			})
		}
		unlock()
	}
	session := &bookmarkSearchSession{Kind: kind, CreatedAt: time.Now()}
	for _, key := range order {
		session.Groups = append(session.Groups, *groups[key])
	}
	token, err := store.NewID()
	if err != nil {
		return result, err
	}
	s.rememberSession(token, session)
	result.Token = token
	for _, g := range session.Groups {
		result.Groups = append(result.Groups, BookmarkSearchGroup{
			Key:           g.Key,
			Name:          g.Name,
			URL:           g.URL,
			InstanceCount: countTargetInstances(g.Targets),
		})
	}
	return result, nil
}

func countTargetInstances(targets []bookmarkTarget) int {
	seen := map[string]bool{}
	for _, t := range targets {
		seen[t.InstanceID] = true
	}
	return len(seen)
}

func (s *BookmarkService) rememberSession(token string, session *bookmarkSearchSession) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	now := time.Now()
	kept := s.sessOrder[:0]
	for _, t := range s.sessOrder {
		if old, ok := s.sessions[t]; ok && now.Sub(old.CreatedAt) < searchSessionTTL {
			kept = append(kept, t)
		} else {
			delete(s.sessions, t)
		}
	}
	s.sessOrder = append(kept, token)
	s.sessions[token] = session
	for len(s.sessOrder) > maxSearchSession {
		delete(s.sessions, s.sessOrder[0])
		s.sessOrder = s.sessOrder[1:]
	}
}

func (s *BookmarkService) lookupSession(token string) (*bookmarkSearchSession, error) {
	s.sessMu.Lock()
	defer s.sessMu.Unlock()
	session, ok := s.sessions[token]
	if !ok || time.Since(session.CreatedAt) > searchSessionTTL {
		return nil, fmt.Errorf("搜索结果已过期，请重新搜索")
	}
	return session, nil
}

// ApplySearchResult aggregates an edit/delete run over search
// targets. Instance identities are never included.
type ApplySearchResult struct {
	UpdatedInstances int `json:"updatedInstances"`
	FailedInstances  int `json:"failedInstances"`
	SkippedInstances int `json:"skippedInstances"`
	AffectedNodes    int `json:"affectedNodes"`
	FailedNodes      int `json:"failedNodes"`
}

// ApplySearchEdit renames/retargets the selected search groups in
// every matching instance. Only profile files change; the library
// is never touched.
func (s *BookmarkService) ApplySearchEdit(operationID string, token string, groupKeys []string, newName string, newURL string) (ApplySearchResult, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.search_edit", "browser_instance", "")
	result, err := s.applySearch(token, groupKeys, func(node map[string]any, kind string) error {
		if kind == model.BookmarkKindBookmark {
			if strings.TrimSpace(newName) != "" {
				chromebookmarks.SetName(node, strings.TrimSpace(newName))
			}
			if strings.TrimSpace(newURL) != "" {
				if err := validateLibraryURL(newURL); err != nil {
					return err
				}
				chromebookmarks.SetURL(node, strings.TrimSpace(newURL))
			}
			return nil
		}
		if strings.TrimSpace(newName) == "" {
			return fmt.Errorf("文件夹名称不能为空")
		}
		chromebookmarks.SetName(node, strings.TrimSpace(newName))
		return nil
	})
	finishOp(op, err)
	return result, err
}

// ApplySearchDelete removes the selected search groups from every
// matching instance (folders with their whole subtree). Only
// profile files change; the library is never touched.
func (s *BookmarkService) ApplySearchDelete(operationID string, token string, groupKeys []string) (ApplySearchResult, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.search_delete", "browser_instance", "")
	result, err := s.applySearch(token, groupKeys, nil)
	finishOp(op, err)
	return result, err
}

// applySearch executes an edit (mutate != nil) or a delete
// (mutate == nil) over the selected groups of a search session.
func (s *BookmarkService) applySearch(token string, groupKeys []string, mutate func(node map[string]any, kind string) error) (ApplySearchResult, error) {
	result := ApplySearchResult{}
	session, err := s.lookupSession(token)
	if err != nil {
		return result, err
	}
	if len(groupKeys) == 0 {
		return result, fmt.Errorf("没有选择搜索结果")
	}
	wanted := make(map[string]bool, len(groupKeys))
	for _, k := range groupKeys {
		wanted[k] = true
	}
	// Collect targets per instance.
	perInstance := map[string][]bookmarkTarget{}
	var instanceOrder []string
	for _, g := range session.Groups {
		if !wanted[g.Key] {
			continue
		}
		for _, t := range g.Targets {
			if _, ok := perInstance[t.InstanceID]; !ok {
				instanceOrder = append(instanceOrder, t.InstanceID)
			}
			perInstance[t.InstanceID] = append(perInstance[t.InstanceID], t)
		}
	}
	if len(instanceOrder) == 0 {
		return result, fmt.Errorf("所选搜索结果没有可操作的目标")
	}
	sort.Strings(instanceOrder)
	for _, instanceID := range instanceOrder {
		s.applyToInstance(instanceID, perInstance[instanceID], session.Kind, mutate, &result)
	}
	return result, nil
}

func (s *BookmarkService) applyToInstance(instanceID string, targets []bookmarkTarget, kind string, mutate func(node map[string]any, kind string) error, result *ApplySearchResult) {
	_, dir, gate, err := s.instances.InstanceBookmarkAccess(instanceID)
	if err != nil {
		result.FailedInstances++
		result.FailedNodes += len(targets)
		return
	}
	if gate != BookmarkGateStopped {
		// State changed since the search (or never was stopped):
		// skip without touching the file.
		result.SkippedInstances++
		return
	}
	unlock := chromebookmarks.LockProfile(dir)
	defer unlock()
	loaded, err := chromebookmarks.Load(dir)
	if err != nil {
		result.FailedInstances++
		result.FailedNodes += len(targets)
		return
	}

	// Phase 1: locate and verify EVERY selected target against the
	// freshly read file before modifying anything. Verified nodes
	// are held by direct reference, so a later rename of a selected
	// parent folder cannot invalidate a selected child's target.
	type verifiedTarget struct {
		node   map[string]any
		parent map[string]any
		depth  int
	}
	wantType := "url"
	if kind == model.BookmarkKindFolder {
		wantType = "folder"
	}
	verified := make([]verifiedTarget, 0, len(targets))
	failedHere := 0
	for _, t := range targets {
		node, parent, rootKey, path, ok := loaded.Locate(t.NodeID)
		if !ok || rootKey != t.RootKey || !samePath(path, t.Path) ||
			chromebookmarks.NameOf(node) != t.Name ||
			chromebookmarks.TypeOf(node) != wantType ||
			(kind == model.BookmarkKindBookmark && chromebookmarks.URLOf(node) != t.URL) {
			failedHere++
			continue
		}
		verified = append(verified, verifiedTarget{node: node, parent: parent, depth: len(t.Path)})
	}

	// Phase 2: apply. Edits mutate the held node references in
	// place. Deletes run deepest-first, so when a folder and its
	// own subfolder are both selected, the subfolder is removed
	// (and counted) before the parent takes the rest of its
	// subtree with it — never reported as a failure.
	changed := false
	applied := 0
	if mutate != nil {
		for _, v := range verified {
			if err := mutate(v.node, kind); err != nil {
				failedHere++
				continue
			}
			applied++
			changed = true
		}
	} else {
		sort.SliceStable(verified, func(i, j int) bool { return verified[i].depth > verified[j].depth })
		for _, v := range verified {
			count := subtreeNodeCount(v.node)
			if v.parent == nil || !chromebookmarks.RemoveFromParent(v.parent, v.node) {
				failedHere++
				continue
			}
			applied += count
			changed = true
		}
	}
	if !changed {
		result.FailedNodes += failedHere
		if failedHere > 0 {
			result.FailedInstances++
		} else {
			result.UpdatedInstances++
		}
		return
	}
	if err := chromebookmarks.Save(dir, loaded); err != nil {
		// Nothing was persisted: the in-memory changes count as
		// failed, not affected.
		result.FailedInstances++
		result.FailedNodes += failedHere + applied
		return
	}
	result.AffectedNodes += applied
	result.FailedNodes += failedHere
	if failedHere > 0 {
		result.FailedInstances++
	} else {
		result.UpdatedInstances++
	}
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func subtreeNodeCount(node map[string]any) int {
	count := 1
	for _, child := range chromebookmarks.Children(node) {
		count += subtreeNodeCount(child)
	}
	return count
}
