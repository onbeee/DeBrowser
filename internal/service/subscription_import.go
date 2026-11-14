package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/mihomoimport"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// The import pipeline (Stage 6 §6/§8/§10): one parsed document is
// applied atomically — vault entries first, the sanitized artifact
// file second, one database transaction last. Any failure before
// the commit compensates the new vault entries and removes the new
// file, and only a committed import cleans up the superseded
// entries and artifact. Existing data of other subscriptions and
// of manual proxies is never touched.

// newImportID returns a random identifier for config rows and
// artifact file names (never derived from user input).
func newImportID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// nodeDedupKey is the stable node identity: owner scope + owner
// ID + a fingerprint of the normalized identity fields (name,
// protocol, server, port — never any secret).
func nodeDedupKey(scope, ownerID string, n mihomoimport.Node) string {
	identity := strings.Join([]string{
		n.Name,
		strings.ToLower(n.Protocol),
		strings.ToLower(strings.TrimSpace(n.Server)),
		fmt.Sprint(n.Port),
	}, "|")
	sum := sha256.Sum256([]byte(identity))
	return scope + ":" + ownerID + ":" + hex.EncodeToString(sum[:])
}

type importOwner struct {
	subscription *model.Subscription // nil for local file imports
	displayName  string              // config display name for file imports
}

// applyImport persists one parsed document for its owner.
func (s *SubscriptionService) applyImport(c context.Context, owner importOwner, parsed *mihomoimport.Parsed) (ImportSummary, error) {
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()

	st := s.proxy.store
	var existingCfg *model.MihomoConfig
	var existingProxies []model.Proxy
	if owner.subscription != nil {
		if cfg, err := st.GetMihomoConfigBySubscription(c, owner.subscription.ID); err == nil {
			existingCfg = &cfg
		} else if !errors.Is(err, store.ErrNotFound) {
			return ImportSummary{}, err
		}
		proxies, err := st.ListProxiesBySubscription(c, owner.subscription.ID)
		if err != nil {
			return ImportSummary{}, err
		}
		existingProxies = proxies
	}

	configID := ""
	if existingCfg != nil {
		configID = existingCfg.ID
	} else {
		id, err := newImportID()
		if err != nil {
			return ImportSummary{}, fmt.Errorf("生成配置标识失败：%w", err)
		}
		configID = id
	}
	configName := owner.displayName
	scope := "file"
	if owner.subscription != nil {
		configName = owner.subscription.Name
		scope = "sub"
	}
	ownerID := configID
	if owner.subscription != nil {
		ownerID = owner.subscription.ID
	}

	// ---- vault first: per-node envelopes, then the config bundle
	var createdRefs []string
	compensate := func() {
		for _, ref := range createdRefs {
			if _, err := s.proxy.vault.DeleteCompensating(ref); err != nil {
				s.log.Warn("compensating vault delete failed after import rollback; orphan left for GC", "error", err)
			}
		}
	}
	type desiredNode struct {
		node     mihomoimport.Node
		dedup    string
		cfgJSON  string
		secretRf string
		matched  *model.Proxy
	}
	existingByDedup := map[string]*model.Proxy{}
	for i := range existingProxies {
		if existingProxies[i].DedupKey != "" {
			existingByDedup[existingProxies[i].DedupKey] = &existingProxies[i]
		}
	}
	desired := make([]desiredNode, 0, len(parsed.Nodes))
	for _, n := range parsed.Nodes {
		cfgJSON, err := json.Marshal(n.Config)
		if err != nil {
			compensate()
			return ImportSummary{}, fmt.Errorf("编码节点配置失败：%w", err)
		}
		port := n.Port
		if _, err := validateProxyFields(n.Name, n.Protocol, n.Server, &port, string(cfgJSON)); err != nil {
			compensate()
			return ImportSummary{}, err
		}
		ref := ""
		if len(n.Secrets) > 0 {
			ref, err = s.proxy.putEnvelope(n.Secrets)
			if err != nil {
				compensate()
				return ImportSummary{}, err
			}
			createdRefs = append(createdRefs, ref)
		}
		dedup := nodeDedupKey(scope, ownerID, n)
		desired = append(desired, desiredNode{
			node: n, dedup: dedup, cfgJSON: string(cfgJSON),
			secretRf: ref, matched: existingByDedup[dedup],
		})
	}
	bundleRef := ""
	if len(parsed.Bundle) > 0 {
		raw, err := json.Marshal(parsed.Bundle)
		if err != nil {
			compensate()
			return ImportSummary{}, fmt.Errorf("编码配置凭据失败：%w", err)
		}
		bundleRef, err = s.proxy.vault.Put(raw)
		if err != nil {
			compensate()
			return ImportSummary{}, fmt.Errorf("保存配置凭据失败：%w", err)
		}
		createdRefs = append(createdRefs, bundleRef)
	}

	// ---- artifact file (new UUID name; the old file stays until
	// the commit lands)
	relPath, digest, err := s.writeArtifact(parsed.SanitizedYAML)
	if err != nil {
		compensate()
		return ImportSummary{}, err
	}
	issuesJSON, _ := json.Marshal(parsed.Issues)
	if parsed.Issues == nil {
		issuesJSON = []byte("[]")
	}

	// ---- one database transaction
	summary := ImportSummary{ConfigID: configID, ConfigName: configName, Format: parsed.Format.String(), Issues: parsed.Issues}
	matchedIDs := map[string]bool{}
	nodeIDByName := map[string]string{}
	var staleRefs []string
	txErr := s.proxy.withTx(c, func(tx *store.Store) error {
		// Name-conflict guard (Stage 6C): BEFORE anything is
		// rebuilt, the incoming document's node names must not
		// collide with the custom nodes the user has attached to
		// this config through the overlay. A conflict aborts the
		// whole transaction here — the previous config, nodes,
		// groups, members, overlay rows and vault references all
		// stay exactly as they were, and the vault entries staged
		// above are compensated by the caller as for any other
		// failed import. (A brand-new config has no overlay rows,
		// so the same check runs and trivially passes.)
		if name, conflict, err := importNameConflict(tx, c, configID, parsed); err != nil {
			return err
		} else if conflict {
			return fmt.Errorf("导入的节点名称「%s」与已加入该配置的自定义节点重名，已拒绝本次导入", name)
		}
		cfgRow := model.MihomoConfig{
			ID: configID, Name: configName, FilePath: relPath, SHA256: digest,
			SecretRef: bundleRef, FormatVersion: 1, SecurityReportJSON: string(issuesJSON),
		}
		if existingCfg != nil {
			if _, err := tx.UpdateMihomoConfig(c, cfgRow); err != nil {
				return err
			}
		} else {
			cfgRow.SourceType = "file"
			if owner.subscription != nil {
				cfgRow.SourceType = "subscription"
				subID := owner.subscription.ID
				cfgRow.SubscriptionID = &subID
			}
			if _, err := tx.CreateMihomoConfig(c, cfgRow); err != nil {
				return err
			}
		}
		for i, d := range desired {
			port := d.node.Port
			row := model.Proxy{
				Name: d.node.Name, Protocol: d.node.Protocol, Address: d.node.Server,
				Port: &port, ConfigJSON: d.cfgJSON, SecretRef: d.secretRf,
				Enabled: true, SortOrder: i, DedupKey: d.dedup,
				SourceNodeName: d.node.Name, ConfigID: &configID,
			}
			if owner.subscription != nil {
				row.Source = "subscription"
				subID := owner.subscription.ID
				row.SubscriptionID = &subID
			} else {
				row.Source = "file"
			}
			if d.matched != nil {
				row.ID = d.matched.ID
				row.Enabled = d.matched.Enabled
				row.SortOrder = d.matched.SortOrder
				if _, err := tx.UpdateProxy(c, row); err != nil {
					return err
				}
				matchedIDs[d.matched.ID] = true
				nodeIDByName[d.node.Name] = d.matched.ID
				if d.matched.SecretRef != "" && d.matched.SecretRef != d.secretRf {
					staleRefs = append(staleRefs, d.matched.SecretRef)
				}
				summary.Updated++
			} else {
				created, err := tx.CreateProxy(c, row)
				if err != nil {
					return err
				}
				nodeIDByName[d.node.Name] = created.ID
				summary.Added++
			}
		}
		for _, old := range existingProxies {
			if !matchedIDs[old.ID] {
				if err := tx.DeleteProxy(c, old.ID); err != nil {
					return err
				}
				if old.SecretRef != "" {
					staleRefs = append(staleRefs, old.SecretRef)
				}
				summary.Removed++
			}
		}
		// Native proxy-groups: fresh group rows, members resolved
		// strictly inside this config (parser already validated
		// that every member resolves here by name).
		groupIDByName := map[string]string{}
		modelGroups := make([]model.MihomoProxyGroup, 0, len(parsed.Groups))
		for i, g := range parsed.Groups {
			gid, err := newImportID()
			if err != nil {
				return err
			}
			groupIDByName[g.Name] = gid
			optsJSON, err := json.Marshal(g.Options)
			if err != nil {
				return fmt.Errorf("编码代理组选项失败：%w", err)
			}
			modelGroups = append(modelGroups, model.MihomoProxyGroup{
				ID: gid, ConfigID: configID, Name: g.Name, Type: g.Type,
				OptionsJSON: string(optsJSON), SortOrder: i,
			})
		}
		for gi, g := range parsed.Groups {
			for mi, member := range g.Members {
				m := model.MihomoProxyGroupMember{SortOrder: mi, MemberName: member}
				switch {
				case nodeIDByName[member] != "":
					id := nodeIDByName[member]
					m.Kind = "proxy"
					m.ProxyID = &id
				case groupIDByName[member] != "":
					id := groupIDByName[member]
					m.Kind = "group"
					m.TargetGroupID = &id
				default:
					m.Kind = "builtin"
				}
				modelGroups[gi].Members = append(modelGroups[gi].Members, m)
			}
		}
		if err := tx.ReplaceProxyGroups(c, configID, modelGroups); err != nil {
			return err
		}
		summary.Groups = len(modelGroups)
		if owner.subscription != nil {
			sub := *owner.subscription
			now := time.Now().UTC().Format("2006-01-02 15:04:05")
			sub.LastUpdatedAt = &now
			sub.LastStatus = "ok"
			sub.LastError = ""
			if _, err := tx.UpdateSubscription(c, sub); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		compensate()
		s.removeArtifact(relPath)
		return ImportSummary{}, mapProxyConstraintError(txErr)
	}

	// ---- post-commit cleanup: superseded vault entries and the
	// old artifact are removed only now.
	if existingCfg != nil {
		if existingCfg.SecretRef != "" && existingCfg.SecretRef != bundleRef {
			staleRefs = append(staleRefs, existingCfg.SecretRef)
		}
		if existingCfg.FilePath != "" && existingCfg.FilePath != relPath {
			s.removeArtifact(existingCfg.FilePath)
		}
	}
	for _, ref := range staleRefs {
		s.deleteRefIfUnreferenced(c, ref)
	}
	return summary, nil
}

// writeArtifact persists the sanitized document under a fresh
// UUID file name inside data/mihomo/configs/, via temp file,
// sync, close and atomic platform replace.
func (s *SubscriptionService) writeArtifact(content []byte) (relPath, digest string, err error) {
	configsDir := filepath.Join(s.layout.Mihomo, "configs")
	if err := os.MkdirAll(configsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("创建配置目录失败：%w", err)
	}
	id, err := newImportID()
	if err != nil {
		return "", "", fmt.Errorf("生成配置标识失败：%w", err)
	}
	name := id + ".yaml"
	tmpName := filepath.Join(configsDir, ".tmp-"+id)
	f, err := os.OpenFile(tmpName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("写入配置文件失败：%w", err)
	}
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("写入配置文件失败：%w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", "", fmt.Errorf("写入配置文件失败：%w", err)
	}
	if err := f.Close(); err != nil {
		return "", "", fmt.Errorf("写入配置文件失败：%w", err)
	}
	if err := platformReplaceFile(tmpName, filepath.Join(configsDir, name)); err != nil {
		return "", "", fmt.Errorf("保存配置文件失败：%w", err)
	}
	sum := sha256.Sum256(content)
	return "configs/" + name, hex.EncodeToString(sum[:]), nil
}

// sanitizeStatusError reduces an error to the short, content-free
// text allowed in subscriptions.last_error.
func sanitizeStatusError(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if r := []rune(msg); len(r) > 200 {
		msg = string(r[:200])
	}
	return msg
}

// markRefreshFailed records a failed refresh without touching the
// subscription's nodes, groups, config or vault entries.
func (s *SubscriptionService) markRefreshFailed(sub model.Subscription, cause error) {
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	sub.LastStatus = "error"
	sub.LastError = sanitizeStatusError(cause)
	_ = s.proxy.withTx(c, func(tx *store.Store) error {
		_, err := tx.UpdateSubscription(c, sub)
		return err
	})
}

// RefreshSubscription performs one manual refresh.
func (s *SubscriptionService) RefreshSubscription(id string) (summary ImportSummary, err error) {
	start := time.Now()
	defer func() { auditOp(s.Auditor, "subscription.refresh", "subscription", id, start, err) }()
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	c, cancel := ctx()
	defer cancel()
	return s.refreshOne(c, id)
}

// refreshOne is the shared manual/automatic refresh path. The
// caller holds refreshMu.
func (s *SubscriptionService) refreshOne(c context.Context, id string) (ImportSummary, error) {
	sub, err := s.proxy.store.GetSubscription(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ImportSummary{}, fmt.Errorf("订阅不存在")
		}
		return ImportSummary{}, err
	}
	if sub.SecretRef == "" {
		err := fmt.Errorf("订阅地址不存在")
		s.markRefreshFailed(sub, err)
		return ImportSummary{}, err
	}
	raw, err := s.proxy.vault.Get(sub.SecretRef)
	if err != nil {
		err = fmt.Errorf("读取订阅地址失败")
		s.markRefreshFailed(sub, err)
		return ImportSummary{}, err
	}
	data, err := s.fetchFn(c, string(raw), sub.AllowLAN)
	if err != nil {
		s.markRefreshFailed(sub, err)
		return ImportSummary{}, err
	}
	parsed, err := mihomoimport.Parse(data)
	if err != nil {
		s.markRefreshFailed(sub, err)
		return ImportSummary{}, err
	}
	summary, err := s.applyImport(c, importOwner{subscription: &sub}, parsed)
	if err != nil {
		s.markRefreshFailed(sub, err)
		return ImportSummary{}, err
	}
	return summary, nil
}

// subscriptionDue decides whether an auto-update subscription is
// due at `now`. The reference time is the last successful update,
// falling back to the row's updated_at (creation / last edit /
// last failed attempt) so failures also respect the interval.
func subscriptionDue(sub model.Subscription, now time.Time) bool {
	if sub.AutoUpdateMinutes <= 0 {
		return false
	}
	base := sub.UpdatedAt
	if sub.LastUpdatedAt != nil && *sub.LastUpdatedAt != "" {
		base = *sub.LastUpdatedAt
	}
	t, err := time.Parse("2006-01-02 15:04:05", base)
	if err != nil {
		if t2, err2 := time.Parse(time.RFC3339, base); err2 == nil {
			t = t2
		} else {
			return true
		}
	}
	return !t.Add(time.Duration(sub.AutoUpdateMinutes) * time.Minute).After(now)
}

// RefreshDueSubscriptions refreshes every due auto-update
// subscription once. A busy refresh lock (manual refresh running)
// skips the sweep; individual failures never stop the others.
func (s *SubscriptionService) RefreshDueSubscriptions(c context.Context) {
	if !s.refreshMu.TryLock() {
		return
	}
	defer s.refreshMu.Unlock()
	subs, err := s.proxy.store.ListSubscriptions(c)
	if err != nil {
		s.log.Warn("listing subscriptions for auto refresh failed", "error", err)
		return
	}
	now := time.Now().UTC()
	for _, sub := range subs {
		if c.Err() != nil {
			return
		}
		if !subscriptionDue(sub, now) {
			continue
		}
		if _, err := s.refreshOne(c, sub.ID); err != nil {
			s.log.Warn("auto refresh failed", "subscription", sub.ID, "error", sanitizeStatusError(err))
		}
	}
}

// ImportLocalConfig imports one user-chosen local file. The file
// dialog is the only source of the path — the frontend can never
// supply a path for the backend to read.
func (s *SubscriptionService) ImportLocalConfig(name string) (summary ImportSummary, err error) {
	start := time.Now()
	defer func() { auditOp(s.Auditor, "config.import", "config", summary.ConfigID, start, err) }()
	if s.FilePicker == nil {
		return ImportSummary{}, fmt.Errorf("本地文件导入不可用")
	}
	path, err := s.FilePicker()
	if err != nil {
		return ImportSummary{}, fmt.Errorf("打开文件选择器失败")
	}
	if path == "" {
		return ImportSummary{Cancelled: true}, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("读取所选文件失败")
	}
	if info.Size() > mihomoimport.MaxInputBytes {
		return ImportSummary{}, fmt.Errorf("文件超过 10 MiB 上限")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ImportSummary{}, fmt.Errorf("读取所选文件失败")
	}
	parsed, err := mihomoimport.Parse(data)
	if err != nil {
		return ImportSummary{}, err
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		base := filepath.Base(path)
		displayName = strings.TrimSuffix(base, filepath.Ext(base))
	}
	if len([]rune(displayName)) > 128 {
		displayName = string([]rune(displayName)[:128])
	}
	c, cancel := ctx()
	defer cancel()
	return s.applyImport(c, importOwner{displayName: displayName}, parsed)
}
