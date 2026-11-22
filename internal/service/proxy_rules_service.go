package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/mihomo"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/store"
)

// This file implements the editable Stage 6C proxy rules and
// core settings: per-ruleset policies, custom top rules, the
// ruleset refresh action, and the editable core settings. Every
// mutation follows the same consistency discipline as the
// binding mutations: build the PROPOSED configuration state,
// push it into the configuration (and the running core, via
// regenerateState's reload + applySelections), and only then
// commit the database change — a failed commit regenerates the
// previous state back.

// applyStateChange regenerates from the proposed state, then
// persists; a persist failure regenerates the previous state
// back so the core and the database never diverge. Callers hold
// s.mu and have validated the change.
func (s *ProxyService) applyStateChange(ctx context.Context, proposed, previous proxyConfigState, persist func() error) error {
	if err := s.regenerateState(ctx, proposed); err != nil {
		return err
	}
	if err := persist(); err != nil {
		if rbErr := s.regenerateState(ctx, previous); rbErr != nil {
			return fmt.Errorf("保存失败且配置回滚失败：%v（保存错误：%w）", rbErr, err)
		}
		return err
	}
	return nil
}

// --- Core settings ---

// Settings storage keys (proxy_settings table).
const (
	settingTestURL             = "test_url"
	settingTestTimeoutMs       = "test_timeout_ms"
	settingProviderIntervalSec = "provider_interval_sec"
	settingDoHServers          = "doh_servers"
	settingLogLevel            = "log_level"
	settingListenerPortStart   = "listener_port_start"
)

// loadProxySettings assembles the effective settings: stored
// entries over the built-in defaults. Unparsable stored values
// fall back to the default for that key (writes are validated,
// so this is only a defensive path).
func (s *ProxyService) loadProxySettings(ctx context.Context) (model.ProxySettings, error) {
	entries, err := s.store.ProxySettingsEntries(ctx)
	if err != nil {
		return model.ProxySettings{}, err
	}
	out := model.DefaultProxySettings()
	if v, ok := entries[settingTestURL]; ok && v != "" {
		out.TestURL = v
	}
	if v, ok := entries[settingTestTimeoutMs]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.TestTimeoutMs = n
		}
	}
	if v, ok := entries[settingProviderIntervalSec]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.ProviderIntervalSec = n
		}
	}
	if v, ok := entries[settingDoHServers]; ok {
		var list []string
		if err := json.Unmarshal([]byte(v), &list); err == nil && len(list) > 0 {
			out.DoHServers = list
		}
	}
	if v, ok := entries[settingLogLevel]; ok && v != "" {
		out.LogLevel = v
	}
	if v, ok := entries[settingListenerPortStart]; ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.ListenerPortStart = n
		}
	}
	return out, nil
}

func proxySettingsEntries(v model.ProxySettings) map[string]string {
	doh, _ := json.Marshal(v.DoHServers)
	return map[string]string{
		settingTestURL:             v.TestURL,
		settingTestTimeoutMs:       strconv.Itoa(v.TestTimeoutMs),
		settingProviderIntervalSec: strconv.Itoa(v.ProviderIntervalSec),
		settingDoHServers:          string(doh),
		settingLogLevel:            v.LogLevel,
		settingListenerPortStart:   strconv.Itoa(v.ListenerPortStart),
	}
}

// ProxySettingsView is the settings dialog's editable form data.
type ProxySettingsView struct {
	TestURL             string   `json:"testUrl"`
	TestTimeoutMs       int      `json:"testTimeoutMs"`
	ProviderIntervalSec int      `json:"providerIntervalSec"`
	DoHServers          []string `json:"dohServers"`
	LogLevel            string   `json:"logLevel"`
	ListenerPortStart   int      `json:"listenerPortStart"`
}

func settingsView(v model.ProxySettings) ProxySettingsView {
	return ProxySettingsView{
		TestURL: v.TestURL, TestTimeoutMs: v.TestTimeoutMs,
		ProviderIntervalSec: v.ProviderIntervalSec, DoHServers: v.DoHServers,
		LogLevel: v.LogLevel, ListenerPortStart: v.ListenerPortStart,
	}
}

func validateProxySettings(v ProxySettingsView) error {
	u, err := url.Parse(strings.TrimSpace(v.TestURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("测速地址无效：请填写完整的 http/https 地址")
	}
	if v.TestTimeoutMs < 100 || v.TestTimeoutMs > 60000 {
		return fmt.Errorf("测速超时须在 100 到 60000 毫秒之间")
	}
	if v.ProviderIntervalSec < 60 || v.ProviderIntervalSec > 2592000 {
		return fmt.Errorf("自动更新间隔须在 60 到 2592000 秒之间")
	}
	if len(v.DoHServers) == 0 || len(v.DoHServers) > 4 {
		return fmt.Errorf("请填写 1 到 4 个 DoH 上游地址")
	}
	for _, srv := range v.DoHServers {
		du, err := url.Parse(strings.TrimSpace(srv))
		if err != nil || du.Scheme != "https" || du.Host == "" {
			return fmt.Errorf("DoH 上游地址无效：%s（须为 https 地址）", srv)
		}
	}
	switch v.LogLevel {
	case "warning", "info", "debug":
	default:
		return fmt.Errorf("无效的日志等级：%s", v.LogLevel)
	}
	if v.ListenerPortStart < 1024 || v.ListenerPortStart > 64535 {
		return fmt.Errorf("实例端口起始值须在 1024 到 64535 之间")
	}
	return nil
}

// GetProxySettings returns the effective core settings.
func (s *ProxyService) GetProxySettings() (ProxySettingsView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	settings, err := s.loadProxySettings(ctx)
	if err != nil {
		return ProxySettingsView{}, err
	}
	return settingsView(settings), nil
}

// UpdateProxySettings validates and saves the core settings.
// Settings the generated configuration embeds (log level, DoH,
// provider interval) take effect through the standard
// regenerate/reload flow; the delay-test values are read at test
// time and the port start steers future allocations.
func (s *ProxyService) UpdateProxySettings(operationID string, v ProxySettingsView) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.settings_update", "mihomo_core", "")
	err := func() error {
		if err := validateProxySettings(v); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		previous := state
		next := model.ProxySettings{
			TestURL: strings.TrimSpace(v.TestURL), TestTimeoutMs: v.TestTimeoutMs,
			ProviderIntervalSec: v.ProviderIntervalSec, LogLevel: v.LogLevel,
			ListenerPortStart: v.ListenerPortStart,
		}
		for _, srv := range v.DoHServers {
			next.DoHServers = append(next.DoHServers, strings.TrimSpace(srv))
		}
		state.settings = next
		if err := s.applyStateChange(ctx, state, previous, func() error {
			return s.store.SetProxySettingsEntries(ctx, proxySettingsEntries(next))
		}); err != nil {
			return fmt.Errorf("核心设置保存失败：%w", err)
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// --- Built-in ruleset policies ---

// RulePolicyView is one built-in ruleset's effective policy.
type RulePolicyView struct {
	Name          string `json:"name"`
	Policy        string `json:"policy"`
	DefaultPolicy string `json:"defaultPolicy"`
}

// GetRulePolicies lists all built-in rulesets with their
// effective policies (stored overrides over the defaults), in
// evaluation order.
func (s *ProxyService) GetRulePolicies() ([]RulePolicyView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	stored, err := s.store.ListRulePolicies(ctx)
	if err != nil {
		return nil, err
	}
	overrides := map[string]string{}
	for _, p := range stored {
		overrides[p.ProviderName] = p.Policy
	}
	effective := mihomo.EffectiveRulePolicies(overrides)
	out := []RulePolicyView{}
	for _, info := range mihomo.BuiltinRuleProviders() {
		out = append(out, RulePolicyView{
			Name: info.Name, Policy: effective[info.Name], DefaultPolicy: info.DefaultPolicy,
		})
	}
	return out, nil
}

func isBuiltinRuleset(name string) bool {
	for _, info := range mihomo.BuiltinRuleProviders() {
		if info.Name == name {
			return true
		}
	}
	return false
}

// SetRulePolicy changes one built-in ruleset's policy.
func (s *ProxyService) SetRulePolicy(operationID string, providerName string, policy string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.rule_policy_set", "mihomo_core", "")
	err := func() error {
		if !isBuiltinRuleset(providerName) {
			return fmt.Errorf("未知的内置规则集：%s", providerName)
		}
		switch policy {
		case model.RulePolicyProxy, model.RulePolicyDirect, model.RulePolicyReject, model.RulePolicyDisabled:
		default:
			return fmt.Errorf("无效的规则集策略：%s", policy)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		previous := state
		proposed := state
		proposed.rulePolicyOverrides = map[string]string{}
		for k, v := range state.rulePolicyOverrides {
			proposed.rulePolicyOverrides[k] = v
		}
		proposed.rulePolicyOverrides[providerName] = policy
		return s.applyStateChange(ctx, proposed, previous, func() error {
			return s.store.UpsertRulePolicy(ctx, providerName, policy)
		})
	}()
	finishOp(op, err)
	return err
}

// ResetRulePolicies removes every policy override ("restore
// defaults").
func (s *ProxyService) ResetRulePolicies(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.rule_policies_reset", "mihomo_core", "")
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		previous := state
		proposed := state
		proposed.rulePolicyOverrides = map[string]string{}
		return s.applyStateChange(ctx, proposed, previous, func() error {
			return s.store.DeleteAllRulePolicies(ctx)
		})
	}()
	finishOp(op, err)
	return err
}

// RefreshRuleProviders asks the running core to re-fetch every
// enabled built-in ruleset now (the manual counterpart of the
// providers' update interval). A ruleset whose fetch fails is
// named in the returned error; the core keeps its cached copy.
func (s *ProxyService) RefreshRuleProviders(operationID string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.rule_providers_refresh", "mihomo_core", "")
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.coreRunning(ctx) {
			return fmt.Errorf("代理核心未开启，无法更新规则集；请先手动开启代理")
		}
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		if !anyRuleModeBindings(state.bindings) {
			return fmt.Errorf("当前没有规则分流模式的实例绑定，无需更新规则集")
		}
		effective := mihomo.EffectiveRulePolicies(state.rulePolicyOverrides)
		var failed []string
		for _, info := range mihomo.BuiltinRuleProviders() {
			if effective[info.Name] == model.RulePolicyDisabled {
				continue
			}
			if err := s.client.RefreshRuleProvider(ctx, info.Name); err != nil {
				failed = append(failed, info.Name)
			}
		}
		if len(failed) > 0 {
			return fmt.Errorf("以下规则集更新失败（核心继续使用已缓存的副本）：%s", strings.Join(failed, "、"))
		}
		return nil
	}()
	finishOp(op, err)
	return err
}

// anyRuleModeBindings reports whether any binding is in rule
// mode (the only mode whose sub-rules reference the rulesets).
func anyRuleModeBindings(bindings []model.InstanceProxyBinding) bool {
	for _, b := range bindings {
		if b.RouteMode == "" || b.RouteMode == model.RouteModeRule {
			return true
		}
	}
	return false
}

// --- Custom top rules ---

// CustomRuleView is one custom top rule as the editor shows it.
type CustomRuleView struct {
	ID        string `json:"id"`
	RuleType  string `json:"ruleType"`
	Value     string `json:"value"`
	Policy    string `json:"policy"`
	Enabled   bool   `json:"enabled"`
	SortOrder int    `json:"sortOrder"`
}

func customRuleView(r model.ProxyCustomRule) CustomRuleView {
	return CustomRuleView{
		ID: r.ID, RuleType: r.RuleType, Value: r.Value, Policy: r.Policy,
		Enabled: r.Enabled, SortOrder: r.SortOrder,
	}
}

// ListCustomRules returns the custom top rules in evaluation
// order.
func (s *ProxyService) ListCustomRules() ([]CustomRuleView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rules, err := s.store.ListCustomRules(ctx)
	if err != nil {
		return nil, err
	}
	out := []CustomRuleView{}
	for _, r := range rules {
		out = append(out, customRuleView(r))
	}
	return out, nil
}

// validateCustomRuleInput checks one custom rule's fields the
// same way the generator will see them, with messages the editor
// can show directly.
func validateCustomRuleInput(ruleType, value, policy string) (string, error) {
	switch ruleType {
	case model.CustomRuleDomainSuffix, model.CustomRuleDomainKeyword, model.CustomRuleIPCIDR:
	default:
		return "", fmt.Errorf("不支持的规则类型：%s", ruleType)
	}
	switch policy {
	case model.CustomRulePolicyProxy, model.CustomRulePolicyDirect, model.CustomRulePolicyReject:
	default:
		return "", fmt.Errorf("无效的规则策略：%s", policy)
	}
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("规则内容不能为空")
	}
	if strings.ContainsAny(v, ", \t\r\n") {
		return "", fmt.Errorf("规则内容不能包含空格或逗号")
	}
	switch ruleType {
	case model.CustomRuleDomainSuffix:
		if strings.Contains(v, "://") || strings.Contains(v, "/") {
			return "", fmt.Errorf("域名后缀请只填写域名部分，例如 example.com")
		}
		if strings.HasPrefix(v, ".") {
			return "", fmt.Errorf("域名后缀不要以点开头，例如 example.com")
		}
	case model.CustomRuleIPCIDR:
		if _, _, err := net.ParseCIDR(v); err != nil {
			return "", fmt.Errorf("IP 段请使用 CIDR 形式，例如 10.0.0.0/8")
		}
	}
	return v, nil
}

// AddCustomRule appends one custom top rule (enabled, last in
// evaluation order).
func (s *ProxyService) AddCustomRule(operationID string, ruleType string, value string, policy string) (CustomRuleView, error) {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.custom_rule_add", "mihomo_core", "")
	view, err := func() (CustomRuleView, error) {
		v, err := validateCustomRuleInput(ruleType, value, policy)
		if err != nil {
			return CustomRuleView{}, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return CustomRuleView{}, err
		}
		id, err := store.NewID()
		if err != nil {
			return CustomRuleView{}, err
		}
		sortOrder := 0
		for _, r := range state.customRules {
			if r.SortOrder >= sortOrder {
				sortOrder = r.SortOrder + 1
			}
		}
		rule := model.ProxyCustomRule{
			ID: id, RuleType: ruleType, Value: v, Policy: policy,
			Enabled: true, SortOrder: sortOrder,
		}
		previous := state
		proposed := state
		proposed.customRules = append(append([]model.ProxyCustomRule(nil), state.customRules...), rule)
		var created model.ProxyCustomRule
		if err := s.applyStateChange(ctx, proposed, previous, func() error {
			var err error
			created, err = s.store.CreateCustomRule(ctx, rule)
			return err
		}); err != nil {
			return CustomRuleView{}, err
		}
		return customRuleView(created), nil
	}()
	finishOp(op, err)
	return view, err
}

// UpdateCustomRule rewrites one custom rule's fields (including
// its enabled flag).
func (s *ProxyService) UpdateCustomRule(operationID string, id string, ruleType string, value string, policy string, enabled bool) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.custom_rule_update", "mihomo_core", "")
	err := func() error {
		v, err := validateCustomRuleInput(ruleType, value, policy)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		idx := -1
		for i, r := range state.customRules {
			if r.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			return fmt.Errorf("要修改的自定义规则不存在")
		}
		previous := state
		proposed := state
		proposed.customRules = append([]model.ProxyCustomRule(nil), state.customRules...)
		proposed.customRules[idx].RuleType = ruleType
		proposed.customRules[idx].Value = v
		proposed.customRules[idx].Policy = policy
		proposed.customRules[idx].Enabled = enabled
		updated := proposed.customRules[idx]
		return s.applyStateChange(ctx, proposed, previous, func() error {
			return s.store.UpdateCustomRule(ctx, updated)
		})
	}()
	finishOp(op, err)
	return err
}

// DeleteCustomRule removes one custom top rule.
func (s *ProxyService) DeleteCustomRule(operationID string, id string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.custom_rule_delete", "mihomo_core", "")
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		previous := state
		proposed := state
		proposed.customRules = nil
		found := false
		for _, r := range state.customRules {
			if r.ID == id {
				found = true
				continue
			}
			proposed.customRules = append(proposed.customRules, r)
		}
		if !found {
			return fmt.Errorf("要删除的自定义规则不存在")
		}
		return s.applyStateChange(ctx, proposed, previous, func() error {
			err := s.store.DeleteCustomRule(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("要删除的自定义规则不存在")
			}
			return err
		})
	}()
	finishOp(op, err)
	return err
}

// ReorderCustomRules rewrites the evaluation order from the
// given ID sequence, which must cover exactly the stored rules.
func (s *ProxyService) ReorderCustomRules(operationID string, ids []string) error {
	op := beginOp(s.Auditor, operationID, "代理", "proxy.custom_rule_reorder", "mihomo_core", "")
	err := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		state, err := s.loadConfigState(ctx)
		if err != nil {
			return err
		}
		if len(ids) != len(state.customRules) {
			return fmt.Errorf("排序列表与现有规则不一致，请刷新后重试")
		}
		byID := map[string]model.ProxyCustomRule{}
		for _, r := range state.customRules {
			byID[r.ID] = r
		}
		previous := state
		proposed := state
		proposed.customRules = nil
		seen := map[string]bool{}
		for i, id := range ids {
			if seen[id] {
				return fmt.Errorf("排序列表包含重复规则，请刷新后重试")
			}
			seen[id] = true
			r, ok := byID[id]
			if !ok {
				return fmt.Errorf("排序列表包含不存在的规则，请刷新后重试")
			}
			r.SortOrder = i
			proposed.customRules = append(proposed.customRules, r)
		}
		return s.applyStateChange(ctx, proposed, previous, func() error {
			return s.store.SetCustomRuleOrder(ctx, ids)
		})
	}()
	finishOp(op, err)
	return err
}
