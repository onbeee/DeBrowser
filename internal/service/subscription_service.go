package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/mihomoimport"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/store"
)

// SubscriptionService (Stage 6) manages subscriptions and imported
// Mihomo configs. It deliberately shares everything with the
// ProxyService — the same store, the same vault and the same
// mutex — so credential consistency rules have exactly one
// coordination point, exactly as the Stage 6 instruction demands.
type SubscriptionService struct {
	proxy   *ProxyService
	layout  paths.Layout
	log     *slog.Logger
	fetcher *subscriptionFetcher

	// Auditor, when set, receives the Stage 5B operation-audit
	// records for subscription and import mutations.
	Auditor *logging.System

	// FilePicker opens the OS file dialog and returns the chosen
	// file's path ("" when the user cancels). The app package
	// wires it; nil means local import is unavailable.
	FilePicker func() (string, error)

	// fetchFn performs one subscription download. It defaults to
	// the SSRF-guarded fetcher; tests substitute a payload source.
	fetchFn func(ctx context.Context, rawURL string, allowLAN bool) (fetchResult, error)

	// refreshMu serialises refreshes (manual and automatic never
	// run concurrently, and the same subscription never refreshes
	// twice at once).
	refreshMu sync.Mutex
}

// NewSubscriptionService builds the service on top of the shared
// ProxyService.
func NewSubscriptionService(proxy *ProxyService, layout paths.Layout, log *slog.Logger) *SubscriptionService {
	s := &SubscriptionService{
		proxy:   proxy,
		layout:  layout,
		log:     log,
		fetcher: newSubscriptionFetcher(),
	}
	s.fetchFn = s.fetcher.fetch
	return s
}

// ---------------------------------------------------------------
// Frontend-safe DTOs. None of these ever carries a full
// subscription URL, a vault reference or credential material.
// ---------------------------------------------------------------

// SubscriptionView is the subscription as the UI may see it.
type SubscriptionView struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	URLHint           string  `json:"urlHint"`
	HasURL            bool    `json:"hasUrl"`
	AllowLAN          bool    `json:"allowLan"`
	AutoUpdateMinutes int     `json:"autoUpdateMinutes"`
	LastUpdatedAt     *string `json:"lastUpdatedAt,omitempty"`
	LastStatus        string  `json:"lastStatus"`
	LastError         string  `json:"lastError"`
	NodeCount         int     `json:"nodeCount"`
	GroupCount        int     `json:"groupCount"`
	HasConfig         bool    `json:"hasConfig"`
	CreatedAt         string  `json:"createdAt"`
	UpdatedAt         string  `json:"updatedAt"`
}

// SubscriptionInput carries the editable subscription fields. On
// update, an empty URL means "keep the stored URL".
type SubscriptionInput struct {
	Name              string `json:"name"`
	URL               string `json:"url"`
	AllowLAN          bool   `json:"allowLan"`
	AutoUpdateMinutes int    `json:"autoUpdateMinutes"`
}

// MihomoConfigView summarises one imported config document.
type MihomoConfigView struct {
	ID             string                       `json:"id"`
	Name           string                       `json:"name"`
	SourceType     string                       `json:"sourceType"`
	SubscriptionID *string                      `json:"subscriptionId,omitempty"`
	SHA256         string                       `json:"sha256"`
	FormatVersion  int                          `json:"formatVersion"`
	NodeCount      int                          `json:"nodeCount"`
	GroupCount     int                          `json:"groupCount"`
	Issues         []mihomoimport.SecurityIssue `json:"issues"`
	CreatedAt      string                       `json:"createdAt"`
	UpdatedAt      string                       `json:"updatedAt"`
}

// ProxyGroupView is one native Mihomo proxy-group with its
// members resolved for display.
type ProxyGroupView struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	Members []GroupMemberView `json:"members"`
}

// GroupMemberView is one group member. ProxyName is the current
// display name of the referenced proxy row (proxy members only).
type GroupMemberView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	ProxyName string `json:"proxyName,omitempty"`
}

// ImportSummary reports what one import/refresh changed.
type ImportSummary struct {
	ConfigID   string                       `json:"configId"`
	ConfigName string                       `json:"configName"`
	Format     string                       `json:"format"`
	Added      int                          `json:"added"`
	Updated    int                          `json:"updated"`
	Removed    int                          `json:"removed"`
	Groups     int                          `json:"groups"`
	Issues     []mihomoimport.SecurityIssue `json:"issues"`
	Cancelled  bool                         `json:"cancelled"`
}

// validateSubscriptionInput checks the fields the backend must
// never trust the frontend with.
func validateSubscriptionInput(input SubscriptionInput, urlRequired bool) (name, rawURL string, err error) {
	name = strings.TrimSpace(input.Name)
	if name == "" {
		return "", "", fmt.Errorf("订阅名称不能为空")
	}
	if len([]rune(name)) > 128 {
		return "", "", fmt.Errorf("订阅名称过长")
	}
	if input.AutoUpdateMinutes != 0 && (input.AutoUpdateMinutes < 15 || input.AutoUpdateMinutes > 7*24*60) {
		return "", "", fmt.Errorf("自动更新间隔不得小于 15 分钟")
	}
	rawURL = strings.TrimSpace(input.URL)
	if rawURL == "" {
		if urlRequired {
			return "", "", fmt.Errorf("订阅地址不能为空")
		}
		return name, "", nil
	}
	if err := validateSubscriptionURL(rawURL); err != nil {
		return "", "", err
	}
	return name, rawURL, nil
}

func (s *SubscriptionService) viewOf(c context.Context, sub model.Subscription) SubscriptionView {
	v := SubscriptionView{
		ID: sub.ID, Name: sub.Name, URLHint: sub.URLHint,
		HasURL: sub.SecretRef != "", AllowLAN: sub.AllowLAN,
		AutoUpdateMinutes: sub.AutoUpdateMinutes,
		LastUpdatedAt:     sub.LastUpdatedAt, LastStatus: sub.LastStatus,
		LastError: sub.LastError, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
	}
	if proxies, err := s.proxy.store.ListProxiesBySubscription(c, sub.ID); err == nil {
		v.NodeCount = len(proxies)
	}
	if cfg, err := s.proxy.store.GetMihomoConfigBySubscription(c, sub.ID); err == nil {
		v.HasConfig = true
		if n, err := s.proxy.store.CountProxyGroups(c, cfg.ID); err == nil {
			v.GroupCount = n
		}
	}
	return v
}

// ListSubscriptions returns every subscription in display form.
func (s *SubscriptionService) ListSubscriptions() ([]SubscriptionView, error) {
	c, cancel := ctx()
	defer cancel()
	subs, err := s.proxy.store.ListSubscriptions(c)
	if err != nil {
		return nil, err
	}
	out := make([]SubscriptionView, 0, len(subs))
	for _, sub := range subs {
		out = append(out, s.viewOf(c, sub))
	}
	return out, nil
}

// CreateSubscription validates the URL, stores it in the vault
// FIRST, and only then writes the metadata row; a failed insert
// is compensated by deleting the pending vault entry.
func (s *SubscriptionService) CreateSubscription(input SubscriptionInput) (view SubscriptionView, err error) {
	start := time.Now()
	defer func() {
		auditOp(s.Auditor, "subscription.create", "subscription", view.ID, start, err)
	}()
	name, rawURL, err := validateSubscriptionInput(input, true)
	if err != nil {
		return SubscriptionView{}, err
	}
	ref, err := s.proxy.vault.Put([]byte(rawURL))
	if err != nil {
		return SubscriptionView{}, fmt.Errorf("保存订阅地址失败：%w", err)
	}
	row := model.Subscription{
		Name: name, URLHint: urlHintOf(rawURL), SecretRef: ref,
		AllowLAN: input.AllowLAN, AutoUpdateMinutes: input.AutoUpdateMinutes,
	}
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	var created model.Subscription
	txErr := s.proxy.withTx(c, func(tx *store.Store) error {
		var err error
		created, err = tx.CreateSubscription(c, row)
		return err
	})
	if txErr != nil {
		if _, cerr := s.proxy.vault.DeleteCompensating(ref); cerr != nil {
			s.log.Warn("compensating vault delete failed after subscription create rollback; orphan left for GC",
				"error", cerr)
		}
		return SubscriptionView{}, mapProxyConstraintError(txErr)
	}
	return s.viewOf(c, created), nil
}

// UpdateSubscription edits a subscription. A non-empty URL rotates
// the vault entry: the new entry is written first, the row is
// switched inside a transaction, and the old entry is removed by
// the guarded cleanup only after the commit. A failed transaction
// compensates the NEW entry and keeps the old one untouched.
func (s *SubscriptionService) UpdateSubscription(id string, input SubscriptionInput) (view SubscriptionView, err error) {
	start := time.Now()
	defer func() { auditOp(s.Auditor, "subscription.update", "subscription", id, start, err) }()
	name, rawURL, err := validateSubscriptionInput(input, false)
	if err != nil {
		return SubscriptionView{}, err
	}
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	existing, err := s.proxy.store.GetSubscription(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return SubscriptionView{}, fmt.Errorf("订阅不存在")
		}
		return SubscriptionView{}, err
	}
	row := existing
	row.Name = name
	row.AllowLAN = input.AllowLAN
	row.AutoUpdateMinutes = input.AutoUpdateMinutes

	newRef := ""
	if rawURL != "" {
		newRef, err = s.proxy.vault.Put([]byte(rawURL))
		if err != nil {
			return SubscriptionView{}, fmt.Errorf("保存订阅地址失败：%w", err)
		}
		row.SecretRef = newRef
		row.URLHint = urlHintOf(rawURL)
	}
	var updated model.Subscription
	txErr := s.proxy.withTx(c, func(tx *store.Store) error {
		var err error
		updated, err = tx.UpdateSubscription(c, row)
		return err
	})
	if txErr != nil {
		if newRef != "" {
			if _, cerr := s.proxy.vault.DeleteCompensating(newRef); cerr != nil {
				s.log.Warn("compensating vault delete failed after subscription update rollback; orphan left for GC",
					"error", cerr)
			}
		}
		return SubscriptionView{}, mapProxyConstraintError(txErr)
	}
	if newRef != "" && existing.SecretRef != "" && existing.SecretRef != newRef {
		s.deleteRefIfUnreferenced(c, existing.SecretRef)
	}
	return s.viewOf(c, updated), nil
}

// DeleteSubscription removes a subscription, all of its proxies
// and its imported config (database cascades), then cleans the
// vault entries that are no longer referenced and deletes the
// config artifact file.
func (s *SubscriptionService) DeleteSubscription(id string) (err error) {
	start := time.Now()
	defer func() { auditOp(s.Auditor, "subscription.delete", "subscription", id, start, err) }()
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	sub, err := s.proxy.store.GetSubscription(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("订阅不存在")
		}
		return err
	}
	refs := []string{sub.SecretRef}
	if proxies, err := s.proxy.store.ListProxiesBySubscription(c, id); err == nil {
		for _, p := range proxies {
			refs = append(refs, p.SecretRef)
		}
	}
	var artifactPath string
	if cfg, err := s.proxy.store.GetMihomoConfigBySubscription(c, id); err == nil {
		refs = append(refs, cfg.SecretRef)
		artifactPath = cfg.FilePath
	}
	txErr := s.proxy.withTx(c, func(tx *store.Store) error {
		return tx.DeleteSubscription(c, id)
	})
	if txErr != nil {
		return mapProxyConstraintError(txErr)
	}
	for _, ref := range refs {
		s.deleteRefIfUnreferenced(c, ref)
	}
	if artifactPath != "" {
		s.removeArtifact(artifactPath)
	}
	return nil
}

// ListConfigs returns summaries of all imported configs.
func (s *SubscriptionService) ListConfigs() ([]MihomoConfigView, error) {
	c, cancel := ctx()
	defer cancel()
	configs, err := s.proxy.store.ListMihomoConfigs(c)
	if err != nil {
		return nil, err
	}
	out := make([]MihomoConfigView, 0, len(configs))
	for _, cfg := range configs {
		v := MihomoConfigView{
			ID: cfg.ID, Name: cfg.Name, SourceType: cfg.SourceType,
			SubscriptionID: cfg.SubscriptionID, SHA256: cfg.SHA256,
			FormatVersion: cfg.FormatVersion, Issues: []mihomoimport.SecurityIssue{},
			CreatedAt: cfg.CreatedAt, UpdatedAt: cfg.UpdatedAt,
		}
		_ = json.Unmarshal([]byte(cfg.SecurityReportJSON), &v.Issues)
		if proxies, err := s.proxy.store.ListProxiesByConfig(c, cfg.ID); err == nil {
			v.NodeCount = len(proxies)
		}
		if n, err := s.proxy.store.CountProxyGroups(c, cfg.ID); err == nil {
			v.GroupCount = n
		}
		out = append(out, v)
	}
	return out, nil
}

// GetConfigGroups returns the native proxy-groups of one config,
// with members resolved for display.
func (s *SubscriptionService) GetConfigGroups(configID string) ([]ProxyGroupView, error) {
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
	proxyNames := map[string]string{}
	if proxies, err := s.proxy.store.ListProxiesByConfig(c, configID); err == nil {
		for _, p := range proxies {
			proxyNames[p.ID] = p.Name
		}
	}
	out := make([]ProxyGroupView, 0, len(groups))
	for _, g := range groups {
		v := ProxyGroupView{ID: g.ID, Name: g.Name, Type: g.Type, Members: []GroupMemberView{}}
		for _, m := range g.Members {
			mv := GroupMemberView{Name: m.MemberName, Kind: m.Kind}
			if m.ProxyID != nil {
				mv.ProxyName = proxyNames[*m.ProxyID]
			}
			v.Members = append(v.Members, mv)
		}
		out = append(out, v)
	}
	return out, nil
}

// DeleteConfig removes a locally imported (file) config together
// with its nodes. Subscription configs are removed by deleting
// their subscription instead.
func (s *SubscriptionService) DeleteConfig(id string) (err error) {
	start := time.Now()
	defer func() { auditOp(s.Auditor, "config.delete", "config", id, start, err) }()
	s.proxy.mu.Lock()
	defer s.proxy.mu.Unlock()
	c, cancel := ctx()
	defer cancel()
	cfg, err := s.proxy.store.GetMihomoConfig(c, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("配置不存在")
		}
		return err
	}
	if cfg.SourceType != "file" {
		return fmt.Errorf("订阅的配置会随订阅一同删除，请删除对应订阅")
	}
	proxies, _ := s.proxy.store.ListProxiesByConfig(c, id)
	refs := []string{cfg.SecretRef}
	for _, p := range proxies {
		refs = append(refs, p.SecretRef)
	}
	txErr := s.proxy.withTx(c, func(tx *store.Store) error {
		for _, p := range proxies {
			if err := tx.DeleteProxy(c, p.ID); err != nil {
				return err
			}
		}
		return tx.DeleteMihomoConfig(c, id)
	})
	if txErr != nil {
		return mapProxyConstraintError(txErr)
	}
	for _, ref := range refs {
		s.deleteRefIfUnreferenced(c, ref)
	}
	s.removeArtifact(cfg.FilePath)
	return nil
}

// deleteRefIfUnreferenced runs the vault's guarded cleanup for one
// ref; failures are logged (safe fields only) and left to GC.
func (s *SubscriptionService) deleteRefIfUnreferenced(c context.Context, ref string) {
	if ref == "" {
		return
	}
	if _, err := s.proxy.vault.DeleteIfUnreferenced(ref, func(r string) (bool, error) {
		return s.proxy.store.SecretRefInUse(c, r)
	}); err != nil {
		s.log.Warn("vault cleanup after subscription change failed; entry left for GC", "error", err)
	}
}

// artifactAbs resolves a stored config file_path (relative to
// data/mihomo/) to an absolute path, proving the result stays
// inside data/mihomo/configs/.
func (s *SubscriptionService) artifactAbs(rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return "", fmt.Errorf("配置文件路径不合法")
	}
	configsDir := filepath.Join(s.layout.Mihomo, "configs")
	abs := filepath.Join(configsDir, filepath.Base(rel))
	if filepath.Dir(abs) != configsDir {
		return "", fmt.Errorf("配置文件路径不合法")
	}
	return abs, nil
}

// removeArtifact deletes one config artifact file; failures are
// logged, never fatal to the committed operation.
func (s *SubscriptionService) removeArtifact(rel string) {
	abs, err := s.artifactAbs(rel)
	if err != nil {
		s.log.Warn("refusing to remove artifact outside the configs directory", "error", err)
		return
	}
	if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
		s.log.Warn("removing stale config artifact failed", "error", err)
	}
}
