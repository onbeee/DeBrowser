package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Custom group-member overlay (Stage 6C review fix): the user
// attaches their own custom nodes to the native proxy-groups of
// an imported config. The overlay lives in its own table, keyed
// by (config, group name, proxy): a subscription refresh rebuilds
// only the imported groups/members, so attachments survive it —
// including a refresh that removes the group (the overlay row is
// kept and reported as orphaned, and it reappears if a later
// refresh brings the group back). Nothing here ever writes to
// the imported structures or to the source document.

// CustomMembershipView is one overlay attachment of one node, for
// the node's detail view. GroupExists is false when the config's
// current document no longer contains the group.
type CustomMembershipView struct {
	ConfigID    string `json:"configId"`
	ConfigName  string `json:"configName"`
	GroupName   string `json:"groupName"`
	GroupExists bool   `json:"groupExists"`
}

// OrphanedCustomGroupView lists the overlay members whose group
// no longer exists in the config's current document.
type OrphanedCustomGroupView struct {
	GroupName string            `json:"groupName"`
	Members   []GroupMemberView `json:"members"`
}

// AddCustomGroupMember attaches one custom node to one native
// group of one config. All legitimacy checks and the write run
// in one transaction under the shared service mutex: the config
// and the group must exist, the node must exist and be a custom
// node (source = 'custom'), and the node must not already be a
// member of that group.
func (s *SubscriptionService) AddCustomGroupMember(configID, groupName, proxyID string) (err error) {
	start := time.Now()
	defer func() {
		auditOp(s.Auditor, "config.group_member.add", "config", configID, start, err)
	}()
	if configID == "" || groupName == "" || proxyID == "" {
		return fmt.Errorf("参数不完整")
	}
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	return s.proxy.withTx(c, func(tx *store.Store) error {
		if _, err := tx.GetMihomoConfig(c, configID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("配置不存在")
			}
			return err
		}
		groups, err := tx.ListProxyGroups(c, configID)
		if err != nil {
			return err
		}
		found := false
		for _, g := range groups {
			if g.Name == groupName {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("该配置没有名为「%s」的代理组", groupName)
		}
		node, err := tx.GetProxy(c, proxyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("节点不存在")
			}
			return err
		}
		if node.Source != "custom" {
			return fmt.Errorf("只有自定义节点可以手动加入代理组")
		}
		if has, err := tx.HasCustomGroupMember(c, configID, groupName, proxyID); err != nil {
			return err
		} else if has {
			return fmt.Errorf("该节点已经是这个代理组的成员")
		}
		n, err := tx.CountCustomGroupMembers(c, configID, groupName)
		if err != nil {
			return err
		}
		return tx.AddCustomGroupMember(c, model.CustomGroupMember{
			ConfigID: configID, GroupName: groupName, ProxyID: proxyID, SortOrder: n,
		})
	})
}

// RemoveCustomGroupMember detaches one user-added member. The
// node itself, its credentials and the imported members are
// never touched.
func (s *SubscriptionService) RemoveCustomGroupMember(configID, groupName, proxyID string) (err error) {
	start := time.Now()
	defer func() {
		auditOp(s.Auditor, "config.group_member.remove", "config", configID, start, err)
	}()
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	if err := s.proxy.store.RemoveCustomGroupMember(c, configID, groupName, proxyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("该自定义成员关系不存在")
		}
		return err
	}
	return nil
}

// ListCustomCandidates returns the custom nodes that can still
// be added to the given group: every source='custom' node except
// the ones already attached to it.
func (s *SubscriptionService) ListCustomCandidates(configID, groupName string) ([]ProxyView, error) {
	c, cancel := ctx()
	defer cancel()
	if _, err := s.proxy.store.GetMihomoConfig(c, configID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("配置不存在")
		}
		return nil, err
	}
	overlay, err := s.proxy.store.ListCustomGroupMembers(c, configID)
	if err != nil {
		return nil, err
	}
	attached := map[string]bool{}
	for _, m := range overlay {
		if m.GroupName == groupName {
			attached[m.ProxyID] = true
		}
	}
	all, err := s.proxy.ListProxies()
	if err != nil {
		return nil, err
	}
	out := []ProxyView{}
	for _, v := range all {
		if v.Source == "custom" && !attached[v.ID] {
			out = append(out, v)
		}
	}
	return out, nil
}

// ListCustomMemberships returns every group attachment of one
// node across all configs, marking attachments whose group has
// meanwhile disappeared from the config's document.
func (s *SubscriptionService) ListCustomMemberships(proxyID string) ([]CustomMembershipView, error) {
	c, cancel := ctx()
	defer cancel()
	rows, err := s.proxy.store.ListCustomMembershipsByProxy(c, proxyID)
	if err != nil {
		return nil, err
	}
	out := []CustomMembershipView{}
	configNames := map[string]string{}
	groupExists := map[string]bool{}
	for _, m := range rows {
		if _, ok := configNames[m.ConfigID]; !ok {
			name := m.ConfigID
			if cfg, err := s.proxy.store.GetMihomoConfig(c, m.ConfigID); err == nil {
				name = cfg.Name
			}
			configNames[m.ConfigID] = name
			if groups, err := s.proxy.store.ListProxyGroups(c, m.ConfigID); err == nil {
				for _, g := range groups {
					groupExists[m.ConfigID+"\x00"+g.Name] = true
				}
			}
		}
		out = append(out, CustomMembershipView{
			ConfigID:    m.ConfigID,
			ConfigName:  configNames[m.ConfigID],
			GroupName:   m.GroupName,
			GroupExists: groupExists[m.ConfigID+"\x00"+m.GroupName],
		})
	}
	return out, nil
}

// ListOrphanedCustomMembers returns, per group name, the overlay
// members of one config whose group no longer exists in the
// config's current document — kept, not silently deleted, so the
// user can review and remove them (and they reappear if the
// group returns in a later refresh).
func (s *SubscriptionService) ListOrphanedCustomMembers(configID string) ([]OrphanedCustomGroupView, error) {
	c, cancel := ctx()
	defer cancel()
	if _, err := s.proxy.store.GetMihomoConfig(c, configID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("配置不存在")
		}
		return nil, err
	}
	groups, err := s.proxy.store.ListProxyGroups(c, configID)
	if err != nil {
		return nil, err
	}
	existing := map[string]bool{}
	for _, g := range groups {
		existing[g.Name] = true
	}
	overlay, err := s.proxy.store.ListCustomGroupMembers(c, configID)
	if err != nil {
		return nil, err
	}
	byGroup := map[string][]GroupMemberView{}
	order := []string{}
	for _, m := range overlay {
		if existing[m.GroupName] {
			continue
		}
		name := m.ProxyID
		if p, err := s.proxy.store.GetProxy(c, m.ProxyID); err == nil {
			name = p.Name
		}
		if _, ok := byGroup[m.GroupName]; !ok {
			order = append(order, m.GroupName)
		}
		byGroup[m.GroupName] = append(byGroup[m.GroupName], GroupMemberView{
			Name: name, Kind: "proxy", ProxyName: name,
			Origin: "custom", ProxyID: m.ProxyID,
		})
	}
	out := []OrphanedCustomGroupView{}
	for _, gname := range order {
		out = append(out, OrphanedCustomGroupView{GroupName: gname, Members: byGroup[gname]})
	}
	return out, nil
}
