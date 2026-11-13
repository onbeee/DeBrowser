package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

func TestProxyStage4Fields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	p, err := s.CreateProxy(ctx, model.Proxy{
		Name: "节点一", Protocol: "socks5", Address: "127.0.0.1", Port: intPtr(1080),
		SortOrder: 5, Source: "manual", DedupKey: "fp-1", SecretRef: "ref-1",
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if p.SortOrder != 5 || p.DedupKey != "fp-1" || p.Source != "manual" {
		t.Fatalf("proxy fields = %+v", p)
	}
	// Test-result fields round-trip through their dedicated writer.
	lat := 42
	if err := s.UpdateProxyTestResult(ctx, p.ID, &lat, true, "2026-11-13 04:00:00"); err != nil {
		t.Fatalf("UpdateProxyTestResult: %v", err)
	}
	got, _ := s.GetProxy(ctx, p.ID)
	if got.LastLatencyMs == nil || *got.LastLatencyMs != 42 ||
		got.LastTestOK == nil || !*got.LastTestOK ||
		got.LastTestedAt == nil || *got.LastTestedAt != "2026-11-13 04:00:00" {
		t.Fatalf("test fields = %+v", got)
	}
	// A failed test clears the latency.
	if err := s.UpdateProxyTestResult(ctx, p.ID, nil, false, "2026-11-13 04:01:00"); err != nil {
		t.Fatalf("UpdateProxyTestResult (fail): %v", err)
	}
	got, _ = s.GetProxy(ctx, p.ID)
	if got.LastLatencyMs != nil || got.LastTestOK == nil || *got.LastTestOK {
		t.Fatalf("after failed test = %+v", got)
	}
}

func TestProxyDedupPartialUnique(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	mk := func(name, key string) error {
		_, err := s.CreateProxy(ctx, model.Proxy{Name: name, Protocol: "http", DedupKey: key})
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

func TestSubscriptionCRUDAndCascade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sub, err := s.CreateSubscription(ctx, model.Subscription{
		Name: "机场甲", URLHint: "https://sub.example.com", SecretRef: "ref-sub",
	})
	if err != nil {
		t.Fatalf("CreateSubscription: %v", err)
	}
	if sub.AllowLAN || sub.AutoUpdateMinutes != 0 {
		t.Fatalf("subscription defaults = %+v, want LAN off / auto-update off", sub)
	}
	p, err := s.CreateProxy(ctx, model.Proxy{
		Name: "订阅节点", Protocol: "vmess", Source: "subscription",
		SubscriptionID: &sub.ID, SecretRef: "ref-node",
	})
	if err != nil {
		t.Fatalf("CreateProxy (subscription): %v", err)
	}
	// Deleting the subscription cascades to its proxies.
	if err := s.DeleteSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := s.GetProxy(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proxy after subscription delete = %v, want ErrNotFound (cascade)", err)
	}
}

func TestSecretRefUsageQueries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.CreateProxy(ctx, model.Proxy{Name: "x", Protocol: "http", SecretRef: "shared-ref"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSubscription(ctx, model.Subscription{Name: "s", SecretRef: "sub-ref"}); err != nil {
		t.Fatal(err)
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
