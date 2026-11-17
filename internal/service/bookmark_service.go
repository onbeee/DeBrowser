package service

import (
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/onbeee/DeBrowser/internal/chromebookmarks"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// BookmarkService implements Stage 9C bookmark management: the
// DeBrowser bookmark library (SQLite) and operations on the real
// bookmarks inside browser profiles (the Chromium Bookmarks file).
// The two data sets are fully independent — library edits never
// touch profiles, and profile operations never write back to the
// library.
//
// Profile files are only read or written while the owning instance
// is confirmed stopped (see InstanceService.InstanceBookmarkAccess):
// a running Chromium browser owns its bookmarks file in memory.
type BookmarkService struct {
	store     *store.Store
	instances *InstanceService
	log       *slog.Logger

	// Auditor, when set, receives structured operation records
	// through the unified recording system.
	Auditor *logging.System

	sessMu    sync.Mutex
	sessions  map[string]*bookmarkSearchSession
	sessOrder []string
}

// NewBookmarkService builds the service over the database and the
// instance service (the source of profile paths and run states).
func NewBookmarkService(db *database.DB, instances *InstanceService, logger *slog.Logger) *BookmarkService {
	if logger == nil {
		logger = slog.Default()
	}
	return &BookmarkService{
		store:     store.New(db),
		instances: instances,
		log:       logger,
		sessions:  map[string]*bookmarkSearchSession{},
	}
}

// ---- library --------------------------------------------------------

type CreateLibraryNodeInput struct {
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	ParentID *string `json:"parentId"`
}

type UpdateLibraryNodeInput struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	URL      string  `json:"url"`
	ParentID *string `json:"parentId"`
}

// ListLibrary returns every library node in stable order; the
// frontend assembles the tree from the parent pointers.
func (s *BookmarkService) ListLibrary() ([]model.BookmarkNode, error) {
	c, cancel := ctx()
	defer cancel()
	return s.store.ListBookmarkNodes(c)
}

func validateLibraryURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("网址格式无效，必须是完整的 http 或 https 网址")
	}
	return nil
}

// libraryIndex indexes a node list by id and by parent.
func libraryIndex(nodes []model.BookmarkNode) (byID map[string]model.BookmarkNode, children map[string][]model.BookmarkNode) {
	byID = make(map[string]model.BookmarkNode, len(nodes))
	children = make(map[string][]model.BookmarkNode)
	for _, n := range nodes {
		byID[n.ID] = n
		key := ""
		if n.ParentID != nil {
			key = *n.ParentID
		}
		children[key] = append(children[key], n)
	}
	return byID, children
}

func parentKey(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// checkParent validates a prospective parent for a node: it must
// exist, be a folder, and — for a move of existing node selfID —
// must not be the node itself or one of its descendants.
func checkParent(byID map[string]model.BookmarkNode, parentID *string, selfID string) error {
	if parentID == nil {
		return nil
	}
	parent, ok := byID[*parentID]
	if !ok {
		return fmt.Errorf("所选文件夹不存在")
	}
	if parent.Kind != model.BookmarkKindFolder {
		return fmt.Errorf("所选上级不是文件夹")
	}
	// Walk up from the prospective parent: meeting selfID means the
	// move would create a cycle.
	for cur := parent; ; {
		if selfID != "" && cur.ID == selfID {
			return fmt.Errorf("不能将文件夹移动到它自身或其子文件夹中")
		}
		if cur.ParentID == nil {
			return nil
		}
		next, ok := byID[*cur.ParentID]
		if !ok {
			return nil
		}
		cur = next
	}
}

func nextSortOrder(siblings []model.BookmarkNode) int {
	max := -1
	for _, n := range siblings {
		if n.SortOrder > max {
			max = n.SortOrder
		}
	}
	return max + 1
}

// CreateLibraryNode adds a bookmark or folder to the library.
func (s *BookmarkService) CreateLibraryNode(operationID string, input CreateLibraryNodeInput) (model.BookmarkNode, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.library_create", "bookmark_node", "")
	node, err := s.createLibraryNode(input)
	finishOp(op, err)
	return node, err
}

func (s *BookmarkService) createLibraryNode(input CreateLibraryNodeInput) (model.BookmarkNode, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return model.BookmarkNode{}, fmt.Errorf("名称不能为空")
	}
	if input.Kind != model.BookmarkKindBookmark && input.Kind != model.BookmarkKindFolder {
		return model.BookmarkNode{}, fmt.Errorf("节点类型无效")
	}
	nodeURL := ""
	if input.Kind == model.BookmarkKindBookmark {
		if err := validateLibraryURL(input.URL); err != nil {
			return model.BookmarkNode{}, err
		}
		nodeURL = strings.TrimSpace(input.URL)
	}
	c, cancel := ctx()
	defer cancel()
	nodes, err := s.store.ListBookmarkNodes(c)
	if err != nil {
		return model.BookmarkNode{}, err
	}
	byID, children := libraryIndex(nodes)
	if err := checkParent(byID, input.ParentID, ""); err != nil {
		return model.BookmarkNode{}, err
	}
	return s.store.CreateBookmarkNode(c, model.BookmarkNode{
		Kind:      input.Kind,
		Name:      name,
		URL:       nodeURL,
		ParentID:  input.ParentID,
		SortOrder: nextSortOrder(children[parentKey(input.ParentID)]),
	})
}

// UpdateLibraryNode renames, retargets or moves a library node.
func (s *BookmarkService) UpdateLibraryNode(operationID string, input UpdateLibraryNodeInput) (model.BookmarkNode, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.library_update", "bookmark_node", input.ID)
	node, err := s.updateLibraryNode(input)
	finishOp(op, err)
	return node, err
}

func (s *BookmarkService) updateLibraryNode(input UpdateLibraryNodeInput) (model.BookmarkNode, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" {
		return model.BookmarkNode{}, fmt.Errorf("名称不能为空")
	}
	c, cancel := ctx()
	defer cancel()
	nodes, err := s.store.ListBookmarkNodes(c)
	if err != nil {
		return model.BookmarkNode{}, err
	}
	byID, _ := libraryIndex(nodes)
	node, ok := byID[input.ID]
	if !ok {
		return model.BookmarkNode{}, fmt.Errorf("书签节点不存在")
	}
	if err := checkParent(byID, input.ParentID, input.ID); err != nil {
		return model.BookmarkNode{}, err
	}
	node.Name = name
	if node.Kind == model.BookmarkKindBookmark {
		if err := validateLibraryURL(input.URL); err != nil {
			return model.BookmarkNode{}, err
		}
		node.URL = strings.TrimSpace(input.URL)
	}
	// Moving to another level appends at the end of that level;
	// staying in place keeps the current order.
	if parentKey(node.ParentID) != parentKey(input.ParentID) {
		_, children := libraryIndex(nodes)
		node.SortOrder = nextSortOrder(children[parentKey(input.ParentID)])
	}
	node.ParentID = input.ParentID
	return s.store.UpdateBookmarkNode(c, node)
}

// DeleteLibraryNodes deletes the selected nodes; folders take
// their whole subtree with them. The caller (frontend) confirms
// the exact scope with the user first.
func (s *BookmarkService) DeleteLibraryNodes(operationID string, ids []string) (int, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.library_delete", "bookmark_node", "")
	count, err := s.deleteLibraryNodes(ids)
	finishOp(op, err)
	return count, err
}

func (s *BookmarkService) deleteLibraryNodes(ids []string) (int, error) {
	if len(ids) == 0 {
		return 0, fmt.Errorf("没有选择要删除的节点")
	}
	c, cancel := ctx()
	defer cancel()
	nodes, err := s.store.ListBookmarkNodes(c)
	if err != nil {
		return 0, err
	}
	byID, children := libraryIndex(nodes)
	// Collect the full deletion set (selected nodes + subtrees),
	// ordered children-first.
	seen := map[string]bool{}
	var ordered []string
	var collect func(id string) error
	collect = func(id string) error {
		if seen[id] {
			return nil
		}
		node, ok := byID[id]
		if !ok {
			return fmt.Errorf("书签节点不存在")
		}
		seen[id] = true
		for _, child := range children[node.ID] {
			if err := collect(child.ID); err != nil {
				return err
			}
		}
		ordered = append(ordered, id)
		return nil
	}
	for _, id := range ids {
		if err := collect(id); err != nil {
			return 0, err
		}
	}
	err = s.store.WithTx(c, func(tx *store.Store) error {
		for _, id := range ordered {
			if err := tx.DeleteBookmarkNode(c, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(ordered), nil
}

// ReorderLibrary persists a new order for the children of one
// level (the root when parentID is nil).
func (s *BookmarkService) ReorderLibrary(operationID string, parentID *string, orderedIDs []string) error {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.library_reorder", "bookmark_node", "")
	err := s.reorderLibrary(parentID, orderedIDs)
	finishOp(op, err)
	return err
}

func (s *BookmarkService) reorderLibrary(parentID *string, orderedIDs []string) error {
	c, cancel := ctx()
	defer cancel()
	nodes, err := s.store.ListBookmarkNodes(c)
	if err != nil {
		return err
	}
	byID, _ := libraryIndex(nodes)
	return s.store.WithTx(c, func(tx *store.Store) error {
		for i, id := range orderedIDs {
			node, ok := byID[id]
			if !ok {
				return fmt.Errorf("书签节点不存在")
			}
			if parentKey(node.ParentID) != parentKey(parentID) {
				return fmt.Errorf("排序节点不属于同一层级")
			}
			node.SortOrder = i
			if _, err := tx.UpdateBookmarkNode(c, node); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- instances ------------------------------------------------------

type BookmarkInstanceInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BookmarkCount int    `json:"bookmarkCount"` // -1 when the count cannot be read
}

// ListBookmarkInstances lists instances with their real bookmark
// counts. A count is only read while the instance is confirmed
// stopped; anything else reports -1 (shown as unknown).
func (s *BookmarkService) ListBookmarkInstances() ([]BookmarkInstanceInfo, error) {
	c, cancel := ctx()
	defer cancel()
	list, err := s.store.ListInstances(c)
	if err != nil {
		return nil, err
	}
	out := make([]BookmarkInstanceInfo, 0, len(list))
	for _, inst := range list {
		info := BookmarkInstanceInfo{ID: inst.ID, Name: inst.Name, BookmarkCount: -1}
		_, dir, gate, gerr := s.instances.InstanceBookmarkAccess(inst.ID)
		if gerr == nil && gate == BookmarkGateStopped {
			unlock := chromebookmarks.LockProfile(dir)
			if file, lerr := chromebookmarks.Load(dir); lerr == nil {
				info.BookmarkCount = file.CountBookmarks()
			}
			unlock()
		}
		out = append(out, info)
	}
	return out, nil
}

// ---- sync to instances ----------------------------------------------

type SyncInstanceResult struct {
	InstanceID   string `json:"instanceId"`
	InstanceName string `json:"instanceName"`
	Status       string `json:"status"` // "success", "failed", "skipped"
	Added        int    `json:"added"`
	Skipped      int    `json:"skipped"`
	Failed       int    `json:"failed"`
	Reason       string `json:"reason"`
}

type SyncBookmarksResult struct {
	Instances        []SyncInstanceResult `json:"instances"`
	SuccessInstances int                  `json:"successInstances"`
	FailedInstances  int                  `json:"failedInstances"`
	SkippedInstances int                  `json:"skippedInstances"`
	AddedBookmarks   int                  `json:"addedBookmarks"`
	SkippedBookmarks int                  `json:"skippedBookmarks"`
	FailedBookmarks  int                  `json:"failedBookmarks"`
}

// SyncBookmarks distributes the selected library nodes (folders
// with their whole subtree, hierarchy preserved) into each chosen
// instance's bookmark bar. Instances are processed independently:
// a running, unconfirmed or unreadable instance never affects the
// others. Within one target folder an identical URL is skipped,
// and same-name folders are reused rather than duplicated.
func (s *BookmarkService) SyncBookmarks(operationID string, nodeIDs []string, instanceIDs []string) (SyncBookmarksResult, error) {
	op := beginOp(s.Auditor, operationID, "书签管理", "bookmark.sync", "browser_instance", "")
	result, err := s.syncBookmarks(nodeIDs, instanceIDs)
	finishOp(op, err)
	return result, err
}

func (s *BookmarkService) syncBookmarks(nodeIDs []string, instanceIDs []string) (SyncBookmarksResult, error) {
	result := SyncBookmarksResult{Instances: []SyncInstanceResult{}}
	if len(nodeIDs) == 0 {
		return result, fmt.Errorf("没有选择要同步的书签")
	}
	if len(instanceIDs) == 0 {
		return result, fmt.Errorf("没有选择目标实例")
	}
	c, cancel := ctx()
	defer cancel()
	nodes, err := s.store.ListBookmarkNodes(c)
	if err != nil {
		return result, err
	}
	byID, children := libraryIndex(nodes)
	selected := make(map[string]bool, len(nodeIDs))
	for _, id := range nodeIDs {
		if _, ok := byID[id]; !ok {
			return result, fmt.Errorf("书签节点不存在")
		}
		selected[id] = true
	}
	// Top-most selected nodes, in library order: a selected node
	// inside a selected folder travels with that folder instead of
	// being added twice.
	var tops []model.BookmarkNode
	for _, n := range nodes {
		if !selected[n.ID] {
			continue
		}
		covered := false
		for p := n.ParentID; p != nil; {
			if selected[*p] {
				covered = true
				break
			}
			parent, ok := byID[*p]
			if !ok {
				break
			}
			p = parent.ParentID
		}
		if !covered {
			tops = append(tops, n)
		}
	}
	sort.SliceStable(tops, func(i, j int) bool { return tops[i].SortOrder < tops[j].SortOrder })

	for _, instanceID := range instanceIDs {
		one := s.syncToInstance(instanceID, tops, children)
		result.Instances = append(result.Instances, one)
		switch one.Status {
		case "success":
			result.SuccessInstances++
		case "failed":
			result.FailedInstances++
		default:
			result.SkippedInstances++
		}
		result.AddedBookmarks += one.Added
		result.SkippedBookmarks += one.Skipped
		result.FailedBookmarks += one.Failed
	}
	return result, nil
}

func (s *BookmarkService) syncToInstance(instanceID string, tops []model.BookmarkNode, children map[string][]model.BookmarkNode) SyncInstanceResult {
	inst, dir, gate, err := s.instances.InstanceBookmarkAccess(instanceID)
	res := SyncInstanceResult{InstanceID: instanceID, InstanceName: inst.Name}
	if err != nil {
		res.Status = "failed"
		res.Reason = err.Error()
		return res
	}
	res.InstanceName = inst.Name
	switch gate {
	case BookmarkGateRunning:
		res.Status = "skipped"
		res.Reason = "实例正在运行，请先关闭浏览器后再同步"
		return res
	case BookmarkGateUnknown:
		res.Status = "skipped"
		res.Reason = "实例运行状态无法确认，已跳过以保护书签数据"
		return res
	}
	unlock := chromebookmarks.LockProfile(dir)
	defer unlock()
	loaded, err := chromebookmarks.Load(dir)
	if err != nil {
		res.Status = "failed"
		res.Reason = "无法确认该实例的书签格式或书签文件不可读（文件不存在、格式不支持或已损坏）"
		return res
	}
	initialCount := loaded.CountBookmarks()
	bar := loaded.EnsureBookmarkBar()
	for _, top := range tops {
		s.addLibraryNode(loaded, bar, top, children, &res)
	}
	if err := chromebookmarks.Save(dir, loaded); err != nil {
		res.Status = "failed"
		res.Reason = "写入书签文件失败：" + err.Error()
		return res
	}
	// Re-read and verify the additions actually landed.
	verify, err := chromebookmarks.Load(dir)
	if err != nil || verify.CountBookmarks() != initialCount+res.Added {
		res.Status = "failed"
		res.Reason = "写入后验证失败，书签可能未完整保存"
		return res
	}
	res.Status = "success"
	return res
}

// addLibraryNode merges one library node (and its subtree) into a
// Chromium folder, reusing same-name folders and skipping URLs the
// target folder already holds.
func (s *BookmarkService) addLibraryNode(file *chromebookmarks.File, target map[string]any, node model.BookmarkNode, children map[string][]model.BookmarkNode, res *SyncInstanceResult) {
	if node.Kind == model.BookmarkKindFolder {
		folder := chromebookmarks.FindChildFolder(target, node.Name)
		if folder == nil {
			folder = file.NewFolder(node.Name)
			chromebookmarks.AppendChild(target, folder)
		}
		for _, child := range children[node.ID] {
			s.addLibraryNode(file, folder, child, children, res)
		}
		return
	}
	if chromebookmarks.ChildHasURL(target, node.URL) {
		res.Skipped++
		return
	}
	chromebookmarks.AppendChild(target, file.NewBookmark(node.Name, node.URL))
	res.Added++
}
