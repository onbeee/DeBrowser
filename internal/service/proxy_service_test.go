package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// newTestProxyService builds a ProxyService over a migrated temp
// database and a real vault in a temp directory (the platform
// protector: AES-GCM keyfile on this dev platform, DPAPI on
// Windows — the service contract is identical either way).
func newTestProxyService(t *testing.T) (*ProxyService, *store.Store, *secrets.Store) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	vault, _, err := secrets.Open(filepath.Join(dir, "secrets.vault"))
	if err != nil {
		t.Fatalf("secrets.Open: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc, err := NewProxyService(db, vault, log)
	if err != nil {
		t.Fatalf("NewProxyService: %v", err)
	}
	return svc, store.New(db), vault
}

func socksInput(name string) CreateProxyInput {
	port := 1080
	return CreateProxyInput{
		Name: name, Protocol: "socks5", Address: "127.0.0.1", Port: &port,
		Secrets: map[string]string{"password": "s3cret-" + name},
	}
}

func TestProxyServiceCreateWithSecretsConsistency(t *testing.T) {
	svc, st, vault := newTestProxyService(t)
	ctx := context.Background()

	view, err := svc.CreateProxy(socksInput("节点一"))
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if !view.HasSecrets || view.CredentialStatus != CredentialOK ||
		len(view.SecretFields) != 1 || view.SecretFields[0] != "password" {
		t.Fatalf("view = %+v", view)
	}
	row, err := st.GetProxy(ctx, view.ID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if row.SecretRef == "" {
		t.Fatal("row has no secret_ref after creating with secrets")
	}
	plain, err := vault.Get(row.SecretRef)
	if err != nil || !strings.Contains(string(plain), "s3cret-节点一") {
		t.Fatalf("vault envelope = %q, %v", plain, err)
	}
	// The plaintext must not appear anywhere in the database row.
	blob := fmt.Sprintf("%+v", row)
	if strings.Contains(blob, "s3cret") {
		t.Fatalf("plaintext leaked into the database row: %s", blob)
	}
}

func TestProxyServiceCreateTxFailureCompensates(t *testing.T) {
	svc, _, vault := newTestProxyService(t)
	svc.withTx = func(ctx context.Context, fn func(tx *store.Store) error) error {
		return errors.New("injected transaction failure")
	}
	if _, err := svc.CreateProxy(socksInput("回滚节点")); err == nil {
		t.Fatal("create succeeded despite injected tx failure")
	}
	if refs := vault.Refs(); len(refs) != 0 {
		t.Fatalf("vault holds %d entries after compensation, want 0 (no orphan for a fresh failure)", len(refs))
	}
}

func TestProxyServiceUpdateSecretLifecycle(t *testing.T) {
	svc, st, vault := newTestProxyService(t)
	ctx := context.Background()

	view, err := svc.CreateProxy(socksInput("生命周期"))
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	row, _ := st.GetProxy(ctx, view.ID)
	ref1 := row.SecretRef

	// Keep: an update without secrets leaves the ref untouched.
	upd := UpdateProxyInput{
		ID: view.ID, Name: "生命周期", Protocol: "socks5",
		Address: "127.0.0.1", Port: view.Port, Enabled: true,
	}
	if _, err := svc.UpdateProxy(upd); err != nil {
		t.Fatalf("UpdateProxy (keep): %v", err)
	}
	row, _ = st.GetProxy(ctx, view.ID)
	if row.SecretRef != ref1 {
		t.Fatalf("keep update changed ref: %q -> %q", ref1, row.SecretRef)
	}

	// Replace: new envelope, ref swap, old entry cleaned.
	upd.Secrets = map[string]string{"password": "new-pass"}
	view2, err := svc.UpdateProxy(upd)
	if err != nil {
		t.Fatalf("UpdateProxy (replace): %v", err)
	}
	row, _ = st.GetProxy(ctx, view.ID)
	if row.SecretRef == "" || row.SecretRef == ref1 {
		t.Fatalf("replace did not swap ref (still %q)", row.SecretRef)
	}
	if vault.Has(ref1) {
		t.Fatal("old vault entry survived a committed ref swap")
	}
	plain, err := vault.Get(row.SecretRef)
	if err != nil || !strings.Contains(string(plain), "new-pass") {
		t.Fatalf("new envelope = %q, %v", plain, err)
	}
	if view2.CredentialStatus != CredentialOK {
		t.Fatalf("view after replace = %+v", view2)
	}

	// Clear: ref removed from the row, vault entry cleaned.
	upd.Secrets = nil
	upd.ClearSecrets = true
	view3, err := svc.UpdateProxy(upd)
	if err != nil {
		t.Fatalf("UpdateProxy (clear): %v", err)
	}
	row, _ = st.GetProxy(ctx, view.ID)
	if row.SecretRef != "" {
		t.Fatalf("clear left ref %q on the row", row.SecretRef)
	}
	if refs := vault.Refs(); len(refs) != 0 {
		t.Fatalf("vault holds %d entries after clear, want 0", len(refs))
	}
	if view3.CredentialStatus != CredentialNone || view3.HasSecrets {
		t.Fatalf("view after clear = %+v", view3)
	}
}

func TestProxyServiceUpdateTxFailureCompensatesNewRef(t *testing.T) {
	svc, st, vault := newTestProxyService(t)
	ctx := context.Background()
	view, err := svc.CreateProxy(socksInput("更新回滚"))
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	row, _ := st.GetProxy(ctx, view.ID)
	ref1 := row.SecretRef

	realWithTx := svc.withTx
	svc.withTx = func(ctx context.Context, fn func(tx *store.Store) error) error {
		return errors.New("injected transaction failure")
	}
	upd := UpdateProxyInput{
		ID: view.ID, Name: "更新回滚", Protocol: "socks5",
		Address: "127.0.0.1", Port: view.Port, Enabled: true,
		Secrets: map[string]string{"password": "never-committed"},
	}
	if _, err := svc.UpdateProxy(upd); err == nil {
		t.Fatal("update succeeded despite injected tx failure")
	}
	svc.withTx = realWithTx
	// The row still points at the old ref, the old entry is
	// intact, and the never-committed new entry was compensated.
	row, _ = st.GetProxy(ctx, view.ID)
	if row.SecretRef != ref1 {
		t.Fatalf("row ref changed after rollback: %q -> %q", ref1, row.SecretRef)
	}
	if refs := vault.Refs(); len(refs) != 1 {
		t.Fatalf("vault holds %d entries, want exactly the old one", len(refs))
	}
	if plain, err := vault.Get(ref1); err != nil || !strings.Contains(string(plain), "s3cret") {
		t.Fatalf("old envelope damaged: %q, %v", plain, err)
	}
}

func TestProxyServiceSharedRefSurvivesUntilLastDelete(t *testing.T) {
	svc, st, vault := newTestProxyService(t)
	ctx := context.Background()
	view, err := svc.CreateProxy(socksInput("共享甲"))
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	row, _ := st.GetProxy(ctx, view.ID)
	ref := row.SecretRef
	// A second database record sharing the same ref (crafted at
	// the store layer, as an import merge could produce).
	other, err := st.CreateProxy(ctx, model.Proxy{
		Name: "共享乙", Protocol: "socks5", Address: "127.0.0.1", SecretRef: ref,
	})
	if err != nil {
		t.Fatalf("CreateProxy (shared): %v", err)
	}
	if err := svc.DeleteProxy(view.ID); err != nil {
		t.Fatalf("DeleteProxy (first): %v", err)
	}
	if !vault.Has(ref) {
		t.Fatal("shared vault entry deleted while another record still references it")
	}
	if err := svc.DeleteProxy(other.ID); err != nil {
		t.Fatalf("DeleteProxy (second): %v", err)
	}
	if vault.Has(ref) {
		t.Fatal("vault entry survived the last referencing record's deletion")
	}
}

func TestProxyServiceCredentialUnavailableIsExplicit(t *testing.T) {
	svc, st, _ := newTestProxyService(t)
	ctx := context.Background()
	p, err := st.CreateProxy(ctx, model.Proxy{
		Name: "坏引用", Protocol: "http", Address: "127.0.0.1", SecretRef: "missing-ref",
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	view, err := svc.GetProxy(p.ID)
	if err != nil {
		t.Fatalf("GetProxy: %v", err)
	}
	if view.CredentialStatus != CredentialUnavailable || !view.HasSecrets || len(view.SecretFields) != 0 {
		t.Fatalf("view = %+v, want explicit credential-unavailable", view)
	}
}

func TestProxyServiceValidation(t *testing.T) {
	svc, _, _ := newTestProxyService(t)
	port := 8080
	cases := []struct {
		name  string
		input CreateProxyInput
	}{
		{"empty name", CreateProxyInput{Protocol: "http", Address: "127.0.0.1", Port: &port}},
		{"bad protocol", CreateProxyInput{Name: "x", Protocol: "socks4", Address: "127.0.0.1", Port: &port}},
		{"missing address", CreateProxyInput{Name: "x", Protocol: "http", Port: &port}},
		{"missing port", CreateProxyInput{Name: "x", Protocol: "http", Address: "127.0.0.1"}},
		{"port zero", CreateProxyInput{Name: "x", Protocol: "http", Address: "127.0.0.1", Port: new(int)}},
		{"secret in config", CreateProxyInput{Name: "x", Protocol: "http", Address: "127.0.0.1", Port: &port, ConfigJSON: `{"password":"leak"}`}},
		{"config not object", CreateProxyInput{Name: "x", Protocol: "http", Address: "127.0.0.1", Port: &port, ConfigJSON: `[1,2]`}},
		{"empty secret value", CreateProxyInput{Name: "x", Protocol: "http", Address: "127.0.0.1", Port: &port, Secrets: map[string]string{"password": ""}}},
	}
	for _, tc := range cases {
		if _, err := svc.CreateProxy(tc.input); err == nil {
			t.Errorf("%s: create succeeded, want validation error", tc.name)
		}
	}
	// direct needs no endpoint and gets none.
	view, err := svc.CreateProxy(CreateProxyInput{Name: "直连", Protocol: "direct", Address: "ignored", Port: &port})
	if err != nil {
		t.Fatalf("CreateProxy (direct): %v", err)
	}
	if view.Address != "" || view.Port != nil {
		t.Fatalf("direct view = %+v, want endpoint cleared", view)
	}
}

func TestProxyServiceGroupsAndTags(t *testing.T) {
	svc, _, _ := newTestProxyService(t)
	g, err := svc.CreateGroup(CreateGroupInput{Name: "工作组"})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if _, err := svc.CreateGroup(CreateGroupInput{Name: "工作组"}); err == nil ||
		!strings.Contains(err.Error(), "分组名称已存在") {
		t.Fatalf("duplicate group error = %v", err)
	}
	port := 7890
	view, err := svc.CreateProxy(CreateProxyInput{
		Name: "带标签", Protocol: "http", Address: "127.0.0.1", Port: &port,
		GroupID: &g.ID, Tags: []string{"  高速 ", "香港", "高速", ""},
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if view.GroupName != "工作组" || len(view.Tags) != 2 {
		t.Fatalf("view = %+v", view)
	}
	groups, err := svc.ListGroups()
	if err != nil || len(groups) != 1 || groups[0].ProxyCount != 1 {
		t.Fatalf("ListGroups = %+v, %v", groups, err)
	}
	tags, err := svc.ListTags()
	if err != nil || len(tags) != 2 {
		t.Fatalf("ListTags = %+v, %v", tags, err)
	}
	if err := svc.RenameTag("香港", "港澳"); err != nil {
		t.Fatalf("RenameTag: %v", err)
	}
	view, _ = svc.GetProxy(view.ID)
	if len(view.Tags) != 2 || view.Tags[0] != "港澳" {
		t.Fatalf("after rename tags = %v", view.Tags)
	}
	if n, err := svc.DeleteTag("港澳"); err != nil || n != 1 {
		t.Fatalf("DeleteTag = %d, %v", n, err)
	}
	if err := svc.DeleteGroup(g.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	view, _ = svc.GetProxy(view.ID)
	if view.GroupID != nil || view.GroupName != "" {
		t.Fatalf("after group delete view = %+v, want ungrouped", view)
	}
}

func TestProxyServiceTestProxy(t *testing.T) {
	svc, _, _ := newTestProxyService(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	view, err := svc.CreateProxy(CreateProxyInput{
		Name: "可达节点", Protocol: "socks5", Address: "127.0.0.1", Port: &port,
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	res, err := svc.TestProxy(view.ID)
	if err != nil {
		t.Fatalf("TestProxy: %v", err)
	}
	if !res.OK || res.LatencyMs == nil || res.Proxy.LastTestOK == nil || !*res.Proxy.LastTestOK {
		t.Fatalf("result = %+v, want recorded success", res)
	}

	// A port with no listener records a failure, not an error.
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPort := ln2.Addr().(*net.TCPAddr).Port
	_ = ln2.Close()
	view2, _ := svc.CreateProxy(CreateProxyInput{
		Name: "不可达节点", Protocol: "socks5", Address: "127.0.0.1", Port: &deadPort,
	})
	res2, err := svc.TestProxy(view2.ID)
	if err != nil {
		t.Fatalf("TestProxy (dead): %v", err)
	}
	if res2.OK || res2.Proxy.LastTestOK == nil || *res2.Proxy.LastTestOK || res2.Proxy.LastLatencyMs != nil {
		t.Fatalf("dead result = %+v, want recorded failure with no latency", res2)
	}

	// direct refuses the test outright.
	direct, _ := svc.CreateProxy(CreateProxyInput{Name: "直连", Protocol: "direct"})
	if _, err := svc.TestProxy(direct.ID); err == nil {
		t.Fatal("TestProxy on direct succeeded, want refusal")
	}
}

func TestProxyServiceGarbageCollectionRespectsLiveAndAge(t *testing.T) {
	svc, st, vault := newTestProxyService(t)
	ctx := context.Background()
	view, err := svc.CreateProxy(socksInput("在用节点"))
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	row, _ := st.GetProxy(ctx, view.ID)
	orphan, err := vault.Put([]byte(`{"password":"orphan"}`))
	if err != nil {
		t.Fatalf("vault.Put orphan: %v", err)
	}
	vault.MarkCommitted(orphan)
	removed, err := svc.CollectSecretGarbage()
	if err != nil {
		t.Fatalf("CollectSecretGarbage: %v", err)
	}
	// The orphan is younger than the 24h gate: nothing is removed,
	// and the live entry is untouchable regardless of age logic.
	if removed != 0 || !vault.Has(orphan) || !vault.Has(row.SecretRef) {
		t.Fatalf("removed = %d, orphan present = %v, live present = %v",
			removed, vault.Has(orphan), vault.Has(row.SecretRef))
	}
}

func TestProxyServicePortRange(t *testing.T) {
	svc, st, _ := newTestProxyService(t)
	ctx := context.Background()
	r, err := svc.GetProxyPortRange()
	if err != nil || r.Start != 19000 || r.End != 19499 {
		t.Fatalf("default range = %+v, %v", r, err)
	}
	if err := st.SetSetting(ctx, "proxy.port_range", "20000-20100"); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.GetProxyPortRange(); r.Start != 20000 || r.End != 20100 {
		t.Fatalf("custom range = %+v", r)
	}
	if err := st.SetSetting(ctx, "proxy.port_range", "garbage"); err != nil {
		t.Fatal(err)
	}
	if r, _ := svc.GetProxyPortRange(); r.Start != 19000 || r.End != 19499 {
		t.Fatalf("malformed range = %+v, want default fallback", r)
	}
}
