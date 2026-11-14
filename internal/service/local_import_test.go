package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/onbeee/DeBrowser/internal/config"
	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// reopenFixture rebuilds the whole service stack over an
// existing layout directory, simulating an application restart.
func reopenFixture(t *testing.T, prev subFixture) subFixture {
	t.Helper()
	layout := prev.layout
	cfg := config.Config{Layout: layout, LogLevel: "info"}
	db, err := database.Open(layout.DatabaseFile)
	if err != nil {
		t.Fatalf("reopen database: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("reopen Migrate: %v", err)
	}
	logs, err := logging.NewSystem(cfg)
	if err != nil {
		t.Fatalf("reopen logging: %v", err)
	}
	vault, _, err := secrets.Open(layout.SecretsFile)
	if err != nil {
		t.Fatalf("reopen vault: %v", err)
	}
	proxySvc, err := NewProxyService(db, vault, logs.Logger())
	if err != nil {
		t.Fatalf("reopen proxy service: %v", err)
	}
	svc := NewSubscriptionService(proxySvc, layout, logs.Logger())
	f := subFixture{svc: svc, proxy: proxySvc, store: store.New(db), vault: vault, layout: layout, db: db}
	t.Cleanup(func() {
		_ = logs.Close()
		_ = db.Close()
	})
	return f
}

// Local-import regression suite (§4.4 of the Stage 6C redo
// instruction), driven through the real ImportLocalConfig entry.
// It includes the regression test for the Windows acceptance
// failure "关联数据不存在或已被删除": a config whose first
// proxy-group references a group defined LATER in the document
// (a forward reference, common in real airport configs) used to
// die on the target_group_id foreign key, because
// ReplaceProxyGroups inserted each group together with its
// members. Groups and members are now inserted in two phases.

const forwardRefYAML = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a}
  - {name: 节点乙, type: trojan, server: b.example.com, port: 443, password: pw-b}
proxy-groups:
  - {name: 主选择, type: select, proxies: [自动组, 节点甲, DIRECT]}
  - {name: 自动组, type: url-test, url: "https://example.com", interval: 300, proxies: [节点甲, 节点乙]}
`

const secondFileYAML = `proxies:
  - {name: 另一节点, type: vmess, server: c.example.com, port: 443, uuid: u-1, alterId: 0}
proxy-groups:
  - {name: 唯一组, type: select, proxies: [另一节点, REJECT]}
`

func importFile(t *testing.T, f subFixture, content, name string) ImportSummary {
	t.Helper()
	file := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	sum, err := f.svc.ImportLocalConfig(name)
	if err != nil {
		t.Fatalf("ImportLocalConfig(%s): %v", name, err)
	}
	return sum
}

func TestLocalImportForwardGroupReference(t *testing.T) {
	f := newSubFixture(t)
	sum := importFile(t, f, forwardRefYAML, "前向引用")
	if sum.Groups != 2 || sum.Added != 2 {
		t.Fatalf("summary = %+v, want 2 groups / 2 nodes", sum)
	}
	groups, err := f.svc.GetConfigGroups(sum.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || groups[0].Name != "主选择" || groups[1].Name != "自动组" {
		t.Fatalf("groups = %+v", groups)
	}
	// The forward member resolved to the later group, by ID.
	found := false
	for _, m := range groups[0].Members {
		if m.Kind == "group" && m.Name == "自动组" {
			found = true
		}
	}
	if !found {
		t.Errorf("forward group member not resolved: %+v", groups[0].Members)
	}
}

func TestLocalImportTwoFilesIsolated(t *testing.T) {
	f := newSubFixture(t)
	s1 := importFile(t, f, forwardRefYAML, "文件一")
	s2 := importFile(t, f, secondFileYAML, "文件二")
	// 3. Distinct config IDs.
	if s1.ConfigID == "" || s2.ConfigID == "" || s1.ConfigID == s2.ConfigID {
		t.Fatalf("config IDs = %q / %q", s1.ConfigID, s2.ConfigID)
	}
	c, cancel := ctx()
	defer cancel()
	// 4 + 6. Nodes belong to their own config only.
	p1, err := f.store.ListProxiesByConfig(c, s1.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := f.store.ListProxiesByConfig(c, s2.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1) != 2 || len(p2) != 1 {
		t.Fatalf("nodes per config = %d / %d, want 2 / 1", len(p1), len(p2))
	}
	for _, p := range p1 {
		if p.ConfigID == nil || *p.ConfigID != s1.ConfigID || p.Source != "file" {
			t.Errorf("node %s misplaced: %+v", p.Name, p)
		}
	}
	for _, p := range p2 {
		if p.ConfigID == nil || *p.ConfigID != s2.ConfigID || p.Source != "file" {
			t.Errorf("node %s misplaced: %+v", p.Name, p)
		}
	}
	// 5 + 7. Groups and their members belong to their own config.
	g1, err := f.store.ListProxyGroups(c, s1.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	g2, err := f.store.ListProxyGroups(c, s2.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(g1) != 2 || len(g2) != 1 || g2[0].Name != "唯一组" {
		t.Fatalf("groups per config = %d / %d", len(g1), len(g2))
	}
	for _, g := range g1 {
		if g.ConfigID != s1.ConfigID {
			t.Errorf("group %s leaked into config %s", g.Name, g.ConfigID)
		}
		for _, m := range g.Members {
			if m.ProxyID != nil {
				ok := false
				for _, p := range p1 {
					if p.ID == *m.ProxyID {
						ok = true
					}
				}
				if !ok {
					t.Errorf("member of %s references a foreign node %s", g.Name, *m.ProxyID)
				}
			}
			if m.TargetGroupID != nil {
				ok := false
				for _, og := range g1 {
					if og.ID == *m.TargetGroupID {
						ok = true
					}
				}
				if !ok {
					t.Errorf("member of %s references a foreign group %s", g.Name, *m.TargetGroupID)
				}
			}
		}
	}
	// Config list shows both.
	configs, err := f.svc.ListConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 2 {
		t.Fatalf("configs = %d, want 2", len(configs))
	}
}

func TestLocalImportParseFailureLeavesNothing(t *testing.T) {
	f := newSubFixture(t)
	importFile(t, f, secondFileYAML, "好文件")
	c, cancel := ctx()
	defer cancel()
	before, _ := f.store.ListMihomoConfigs(c)
	file := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(file, []byte("proxies:\n  - {name: x, type: bogusproto, server: s, port: 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	if _, err := f.svc.ImportLocalConfig("坏文件"); err == nil {
		t.Fatal("parse-failing import was accepted")
	}
	after, _ := f.store.ListMihomoConfigs(c)
	if len(after) != len(before) {
		t.Errorf("configs changed by failed import: %d -> %d", len(before), len(after))
	}
}

func TestLocalImportTxFailureRollsBack(t *testing.T) {
	f := newSubFixture(t)
	realTx := f.proxy.withTx
	f.proxy.withTx = func(context.Context, func(tx *store.Store) error) error {
		return errors.New("injected tx failure")
	}
	defer func() { f.proxy.withTx = realTx }()
	refsBefore := len(f.vault.Refs())
	file := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(file, []byte(forwardRefYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	if _, err := f.svc.ImportLocalConfig("回滚"); err == nil {
		t.Fatal("import succeeded despite tx failure")
	}
	c, cancel := ctx()
	defer cancel()
	if cfgs, _ := f.store.ListMihomoConfigs(c); len(cfgs) != 0 {
		t.Errorf("configs after rollback = %d, want 0", len(cfgs))
	}
	if ps, _ := f.proxy.ListProxies(); len(ps) != 0 {
		t.Errorf("proxies after rollback = %d, want 0", len(ps))
	}
	if got := len(f.vault.Refs()); got != refsBefore {
		t.Errorf("vault entries = %d, want %d (compensation)", got, refsBefore)
	}
	// The artifact file must be gone too.
	entries, _ := os.ReadDir(filepath.Join(f.layout.Mihomo, "configs"))
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".yaml" {
			t.Errorf("artifact survived rollback: %s", e.Name())
		}
	}
}

// TestLocalImportSurvivesRestart: imported data persists across
// a database close/reopen (a fresh fixture over the same layout
// would regenerate paths; this test rebuilds services manually
// on the same directory).
func TestLocalImportSurvivesRestart(t *testing.T) {
	f := newSubFixture(t)
	sum := importFile(t, f, forwardRefYAML, "持久化")
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	f2 := reopenFixture(t, f)
	configs, err := f2.svc.ListConfigs()
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].ID != sum.ConfigID || configs[0].NodeCount != 2 || configs[0].GroupCount != 2 {
		t.Fatalf("configs after restart = %+v", configs)
	}
	groups, err := f2.svc.GetConfigGroups(sum.ConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups after restart = %d, want 2", len(groups))
	}
}

// TestLocalImportTwoStepPickedFile: the create dialog's flow —
// pick a file first (only its name reaches the frontend), then
// import it under a chosen display name.
func TestLocalImportTwoStepPickedFile(t *testing.T) {
	f := newSubFixture(t)
	// Importing before picking is an error, not a silent no-op.
	if _, err := f.svc.ImportPickedLocalConfig("未选择"); err == nil {
		t.Fatal("import without a picked file succeeded")
	}
	file := filepath.Join(t.TempDir(), "airport.yaml")
	if err := os.WriteFile(file, []byte(forwardRefYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	name, err := f.svc.SelectLocalConfigFile()
	if err != nil || name != "airport.yaml" {
		t.Fatalf("SelectLocalConfigFile = %q, %v", name, err)
	}
	sum, err := f.svc.ImportPickedLocalConfig("两步导入")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Added != 2 || sum.Groups != 2 {
		t.Fatalf("summary = %+v", sum)
	}
	configs, err := f.svc.ListConfigs()
	if err != nil || len(configs) != 1 || configs[0].Name != "两步导入" {
		t.Fatalf("configs = %+v, %v", configs, err)
	}
	// The pick is consumed by a successful import.
	if _, err := f.svc.ImportPickedLocalConfig("再次"); err == nil {
		t.Fatal("picked file was not consumed by the import")
	}
}
