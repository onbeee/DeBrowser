package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

// The tests in this file cover Stage 4-era tables (proxies,
// subscriptions, settings) as HISTORICAL data: after the PR #9 scope
// cleanup the store exposes no proxy/subscription write API, so rows
// are seeded and removed with raw SQL — exactly the state a migrated
// user database is in — and only schema behaviour (CHECK, partial
// unique, cascades) plus the vault ref queries are asserted.

func TestHistoricalProxyRowRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if _, err := s.q.ExecContext(ctx, `INSERT INTO proxies
		(id, name, protocol, address, port, sort_order, source, dedup_key,
		 secret_ref, last_latency_ms, last_test_ok, last_tested_at)
		VALUES ('hp1', '节点一', 'socks5', '127.0.0.1', 1080, 5, 'custom',
		 'fp-1', 'ref-1', 42, 1, '2026-11-13 04:00:00')`); err != nil {
		t.Fatalf("insert historical proxy: %v", err)
	}
	var (
		sortOrder int
		source    string
		dedup     string
		latency   int
		testOK    int
		testedAt  string
	)
	if err := s.q.QueryRowContext(ctx, `SELECT sort_order, source, dedup_key,
		last_latency_ms, last_test_ok, last_tested_at FROM proxies WHERE id = 'hp1'`).
		Scan(&sortOrder, &source, &dedup, &latency, &testOK, &testedAt); err != nil {
		t.Fatalf("read historical proxy: %v", err)
	}
	if sortOrder != 5 || source != "custom" || dedup != "fp-1" ||
		latency != 42 || testOK != 1 || testedAt != "2026-11-13 04:00:00" {
		t.Fatalf("historical fields = %d %q %q %d %d %q",
			sortOrder, source, dedup, latency, testOK, testedAt)
	}
}

func TestProxyDedupPartialUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mk := func(id, key string) error {
		_, err := s.q.ExecContext(ctx,
			`INSERT INTO proxies (id, name, protocol, dedup_key) VALUES (?, ?, 'http', ?)`,
			id, id, key)
		return err
	}
	// Empty dedup keys never collide.
	if err := mk("a", ""); err != nil {
		t.Fatalf("empty key a: %v", err)
	}
	if err := mk("b", ""); err != nil {
		t.Fatalf("empty key b: %v", err)
	}
	if err := mk("c", "same-fp"); err != nil {
		t.Fatalf("first keyed: %v", err)
	}
	if err := mk("d", "same-fp"); err == nil {
		t.Fatal("duplicate non-empty dedup_key accepted, want partial-unique violation")
	}
}

func TestSubscriptionDeleteCascadesToProxies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.q.ExecContext(ctx, `INSERT INTO subscriptions
		(id, name, url_hint, secret_ref) VALUES ('sub1', '机场甲', 'https://sub.example.com', 'ref-sub')`); err != nil {
		t.Fatalf("insert historical subscription: %v", err)
	}
	var allowLAN, autoUpdate int
	if err := s.q.QueryRowContext(ctx,
		`SELECT allow_lan, auto_update_minutes FROM subscriptions WHERE id = 'sub1'`).
		Scan(&allowLAN, &autoUpdate); err != nil {
		t.Fatalf("read subscription defaults: %v", err)
	}
	if allowLAN != 0 || autoUpdate != 0 {
		t.Fatalf("subscription defaults = %d/%d, want LAN off / auto-update off", allowLAN, autoUpdate)
	}
	if _, err := s.q.ExecContext(ctx, `INSERT INTO proxies
		(id, name, protocol, source, subscription_id, secret_ref)
		VALUES ('sp1', '订阅节点', 'vmess', 'subscription', 'sub1', 'ref-node')`); err != nil {
		t.Fatalf("insert subscription proxy: %v", err)
	}
	// Schema cascade: deleting the subscription row removes its
	// proxies. (No application code deletes subscriptions any more;
	// this pins the historical schema behaviour.)
	if _, err := s.q.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = 'sub1'`); err != nil {
		t.Fatalf("delete subscription row: %v", err)
	}
	var n int
	if err := s.q.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM proxies WHERE id = 'sp1'`).Scan(&n); err != nil {
		t.Fatalf("count proxies after cascade: %v", err)
	}
	if n != 0 {
		t.Fatalf("proxies after subscription delete = %d, want 0 (cascade)", n)
	}
}

func TestSecretRefUsageQueries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedHistoricalProxy(t, s, "srp1", "x", "http", "shared-ref")
	if _, err := s.q.ExecContext(ctx, `INSERT INTO subscriptions
		(id, name, secret_ref) VALUES ('srs1', 's', 'sub-ref')`); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	for ref, want := range map[string]bool{"shared-ref": true, "sub-ref": true, "other": false, "": false} {
		got, err := s.SecretRefInUse(ctx, ref)
		if err != nil || got != want {
			t.Errorf("SecretRefInUse(%q) = %v, %v; want %v", ref, got, err, want)
		}
	}
	all, err := s.AllSecretRefs(ctx)
	if err != nil || len(all) != 2 || !all["shared-ref"] || !all["sub-ref"] {
		t.Fatalf("AllSecretRefs = %v, %v", all, err)
	}
}

func TestSettingsKV(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.GetSetting(ctx, "proxy.port_range"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key = %v, want ErrNotFound", err)
	}
	if err := s.SetSetting(ctx, "proxy.port_range", "19000-19499"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, err := s.GetSetting(ctx, "proxy.port_range")
	if err != nil || v != "19000-19499" {
		t.Fatalf("GetSetting = %q, %v", v, err)
	}
	if err := s.SetSetting(ctx, "proxy.port_range", "20000-20100"); err != nil {
		t.Fatalf("SetSetting replace: %v", err)
	}
	if v, _ := s.GetSetting(ctx, "proxy.port_range"); v != "20000-20100" {
		t.Fatalf("after replace = %q", v)
	}
	if err := s.DeleteSetting(ctx, "proxy.port_range"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}
	if _, err := s.GetSetting(ctx, "proxy.port_range"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete = %v, want ErrNotFound", err)
	}
}

func TestInstanceProxyListenPort(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	port := 19001
	in, err := s.CreateInstance(ctx, model.Instance{
		Name: "带端口实例", UserDataDir: "profiles/x1", ProxyListenPort: &port,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if in.ProxyListenPort == nil || *in.ProxyListenPort != 19001 {
		t.Fatalf("listen port = %+v", in.ProxyListenPort)
	}
	// The UNIQUE constraint refuses a second instance on the port.
	if _, err := s.CreateInstance(ctx, model.Instance{
		Name: "冲突实例", UserDataDir: "profiles/x2", ProxyListenPort: &port,
	}); err == nil {
		t.Fatal("duplicate proxy_listen_port accepted, want UNIQUE violation")
	}
	// NULL ports never collide.
	for _, name := range []string{"无端口甲", "无端口乙"} {
		if _, err := s.CreateInstance(ctx, model.Instance{
			Name: name, UserDataDir: "profiles/" + name,
		}); err != nil {
			t.Fatalf("NULL listen port instance %s: %v", name, err)
		}
	}
}
