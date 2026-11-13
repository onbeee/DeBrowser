package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

func TestProxyGroupCRUDAndUniqueName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	g, err := s.CreateProxyGroup(ctx, model.ProxyGroup{Name: "主力", SortOrder: 2})
	if err != nil {
		t.Fatalf("CreateProxyGroup: %v", err)
	}
	if g.ID == "" || g.Name != "主力" {
		t.Fatalf("group = %+v", g)
	}
	if _, err := s.CreateProxyGroup(ctx, model.ProxyGroup{Name: "主力"}); err == nil {
		t.Fatal("duplicate group name accepted, want UNIQUE violation")
	}
	g.SortOrder = 1
	updated, err := s.UpdateProxyGroup(ctx, g)
	if err != nil || updated.SortOrder != 1 {
		t.Fatalf("UpdateProxyGroup = %+v, %v", updated, err)
	}
	list, err := s.ListProxyGroups(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProxyGroups = %v, %v", list, err)
	}
	if err := s.DeleteProxyGroup(ctx, g.ID); err != nil {
		t.Fatalf("DeleteProxyGroup: %v", err)
	}
	if err := s.DeleteProxyGroup(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound", err)
	}
}

func TestProxyStage4FieldsAndGroupDeleteSetNull(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	g, _ := s.CreateProxyGroup(ctx, model.ProxyGroup{Name: "分组甲"})
	p, err := s.CreateProxy(ctx, model.Proxy{
		Name: "节点一", Protocol: "socks5", Address: "127.0.0.1", Port: intPtr(1080),
		GroupID: &g.ID, SortOrder: 5, Source: "manual", DedupKey: "fp-1", SecretRef: "ref-1",
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if p.GroupID == nil || *p.GroupID != g.ID || p.SortOrder != 5 || p.DedupKey != "fp-1" || p.Source != "manual" {
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
	// Deleting the group ungroups the proxy instead of deleting it.
	if err := s.DeleteProxyGroup(ctx, g.ID); err != nil {
		t.Fatalf("DeleteProxyGroup: %v", err)
	}
	got, err = s.GetProxy(ctx, p.ID)
	if err != nil {
		t.Fatalf("proxy vanished with its group: %v", err)
	}
	if got.GroupID != nil {
		t.Fatalf("group_id = %v, want NULL after group delete", *got.GroupID)
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

func TestProxyTagsLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	p1, _ := s.CreateProxy(ctx, model.Proxy{Name: "p1", Protocol: "http"})
	p2, _ := s.CreateProxy(ctx, model.Proxy{Name: "p2", Protocol: "http"})

	if err := s.SetProxyTags(ctx, p1.ID, []string{"高速", "香港"}); err != nil {
		t.Fatalf("SetProxyTags: %v", err)
	}
	if err := s.SetProxyTags(ctx, p2.ID, []string{"高速"}); err != nil {
		t.Fatalf("SetProxyTags p2: %v", err)
	}
	tags, err := s.TagsForProxy(ctx, p1.ID)
	if err != nil || len(tags) != 2 {
		t.Fatalf("TagsForProxy = %v, %v", tags, err)
	}
	byProxy, err := s.TagsForProxies(ctx, []string{p1.ID, p2.ID})
	if err != nil || len(byProxy[p1.ID]) != 2 || len(byProxy[p2.ID]) != 1 {
		t.Fatalf("TagsForProxies = %v, %v", byProxy, err)
	}
	counts, err := s.ListTagCounts(ctx)
	if err != nil || len(counts) != 2 || counts[1].Tag != "高速" || counts[1].Count != 2 {
		t.Fatalf("ListTagCounts = %+v, %v", counts, err)
	}
	// Rename with a collision: p1 already has 香港， renaming 高速 ->
	// 香港 must leave exactly one 香港 on p1 and one on p2.
	if err := s.RenameTagEverywhere(ctx, "高速", "香港"); err != nil {
		t.Fatalf("RenameTagEverywhere: %v", err)
	}
	tags, _ = s.TagsForProxy(ctx, p1.ID)
	if len(tags) != 1 || tags[0] != "香港" {
		t.Fatalf("after colliding rename p1 tags = %v", tags)
	}
	// Replacement semantics of SetProxyTags.
	if err := s.SetProxyTags(ctx, p1.ID, []string{"新标签"}); err != nil {
		t.Fatalf("SetProxyTags replace: %v", err)
	}
	tags, _ = s.TagsForProxy(ctx, p1.ID)
	if len(tags) != 1 || tags[0] != "新标签" {
		t.Fatalf("after replace p1 tags = %v", tags)
	}
	// Deleting a proxy cascades its tag rows.
	if err := s.DeleteProxy(ctx, p1.ID); err != nil {
		t.Fatalf("DeleteProxy: %v", err)
	}
	if tags, _ := s.TagsForProxy(ctx, p1.ID); len(tags) != 0 {
		t.Fatalf("tags survived proxy deletion: %v", tags)
	}
	// Delete a tag everywhere.
	n, err := s.DeleteTagEverywhere(ctx, "香港")
	if err != nil || n != 1 {
		t.Fatalf("DeleteTagEverywhere = %d, %v; want 1", n, err)
	}
}

func TestSubscriptionCRUDAndCascade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	g, _ := s.CreateProxyGroup(ctx, model.ProxyGroup{Name: "订阅组"})
	sub, err := s.CreateSubscription(ctx, model.Subscription{
		Name: "机场甲", URLHint: "https://sub.example.com", SecretRef: "ref-sub",
		GroupID: &g.ID,
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
	// Deleting the subscription cascades to its proxies only.
	if err := s.DeleteSubscription(ctx, sub.ID); err != nil {
		t.Fatalf("DeleteSubscription: %v", err)
	}
	if _, err := s.GetProxy(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("proxy after subscription delete = %v, want ErrNotFound (cascade)", err)
	}
	if _, err := s.GetProxyGroup(ctx, g.ID); err != nil {
		t.Fatalf("group wrongly removed: %v", err)
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
