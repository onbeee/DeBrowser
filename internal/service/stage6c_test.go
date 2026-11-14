package service

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUnifiedSourcesCoexistAndDeleteIsolated (Stage 6C): the
// three proxy sources — subscription, local file, custom — live
// in one store with the unified vocabulary, and deleting one
// source never touches the nodes of the others.
func TestUnifiedSourcesCoexistAndDeleteIsolated(t *testing.T) {
	f := newSubFixture(t)

	// Custom node (no subscription, no config).
	port := 1080
	custom, err := f.proxy.CreateProxy(CreateProxyInput{
		Name: "自定义一", Protocol: "socks5", Address: "127.0.0.1", Port: &port,
		Secrets: map[string]string{"password": "pw-custom"},
	})
	if err != nil {
		t.Fatalf("CreateProxy: %v", err)
	}
	if custom.Source != "custom" || custom.ConfigID != nil || custom.SubscriptionID != nil {
		t.Fatalf("custom view = %+v", custom)
	}
	if !f.vaultHas(customSecretRef(t, f, custom.ID)) {
		t.Fatal("custom node credentials not in vault")
	}

	// Subscription nodes.
	sub := f.createSub(t)
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(sub.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	// File-imported nodes via the native picker seam.
	file := filepath.Join(t.TempDir(), "airport.yaml")
	if err := os.WriteFile(file, []byte(subFeedV2), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	sum, err := f.svc.ImportLocalConfig("本地一")
	if err != nil {
		t.Fatalf("ImportLocalConfig: %v", err)
	}

	all, err := f.proxy.ListProxies()
	if err != nil {
		t.Fatal(err)
	}
	bySource := map[string]int{}
	for _, v := range all {
		bySource[v.Source]++
		if v.Source == "file" && (v.ConfigID == nil || *v.ConfigID != sum.ConfigID) {
			t.Fatalf("file node view lacks its config id: %+v", v)
		}
		if v.Source == "subscription" && (v.SubscriptionID == nil || *v.SubscriptionID != sub.ID) {
			t.Fatalf("subscription node view lacks its subscription id: %+v", v)
		}
	}
	if bySource["custom"] != 1 || bySource["subscription"] != 2 || bySource["file"] != 2 {
		t.Fatalf("sources = %v, want 1 custom + 2 subscription + 2 file", bySource)
	}

	// Deleting the subscription removes only its own nodes.
	if err := f.svc.DeleteSubscription(sub.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting the file config removes only its own nodes.
	if err := f.svc.DeleteConfig(sum.ConfigID); err != nil {
		t.Fatal(err)
	}
	left, err := f.proxy.ListProxies()
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0].ID != custom.ID || left[0].Source != "custom" {
		t.Fatalf("after source deletions, remaining = %+v, want only the custom node", left)
	}
	if !f.vaultHas(customSecretRef(t, f, custom.ID)) {
		t.Fatal("custom node credentials lost during other sources' deletion")
	}
}

func customSecretRef(t *testing.T, f subFixture, proxyID string) string {
	t.Helper()
	c, cancel := ctx()
	defer cancel()
	row, err := f.store.GetProxy(c, proxyID)
	if err != nil {
		t.Fatal(err)
	}
	return row.SecretRef
}

// TestRenamedImportedNodeKeepsGroupMembership (Stage 6C): group
// members bind to the stable proxy ID, so renaming an imported
// node's display name re-labels the member view without ever
// rebinding it to a different node.
func TestRenamedImportedNodeKeepsGroupMembership(t *testing.T) {
	f := newSubFixture(t)
	sub := f.createSub(t)
	f.stubFeed(subFeedV1)
	sum, err := f.svc.RefreshSubscription(sub.ID)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	c, cancel := ctx()
	defer cancel()
	nodes, err := f.store.ListProxiesBySubscription(c, sub.ID)
	if err != nil || len(nodes) == 0 {
		t.Fatalf("nodes = %v, err %v", nodes, err)
	}
	target := nodes[0]
	if _, err := f.proxy.UpdateProxy(UpdateProxyInput{
		ID: target.ID, Name: "改名后的节点", Protocol: target.Protocol,
		Address: target.Address, Port: target.Port, ConfigJSON: target.ConfigJSON,
		SortOrder: target.SortOrder, Enabled: target.Enabled,
	}); err != nil {
		t.Fatalf("UpdateProxy rename: %v", err)
	}
	// Store level: the member row still binds the same proxy ID.
	stored, err := f.store.ListProxyGroups(c, sum.ConfigID)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored groups = %+v, err %v", stored, err)
	}
	found := false
	for _, m := range stored[0].Members {
		if m.Kind == "proxy" && m.ProxyID != nil && *m.ProxyID == target.ID {
			found = true
			if m.MemberName != target.SourceNodeName {
				t.Errorf("member source name = %q, want unchanged %q", m.MemberName, target.SourceNodeName)
			}
		}
		if m.Kind == "proxy" && m.ProxyID == nil {
			t.Errorf("proxy member lost its binding after rename: %+v", m)
		}
	}
	if !found {
		t.Fatalf("renamed node no longer a member of its group: %+v", stored[0].Members)
	}
	// View level: the member still resolves to the renamed node.
	groups, err := f.svc.GetConfigGroups(sum.ConfigID)
	if err != nil || len(groups) != 1 {
		t.Fatalf("groups = %+v, err %v", groups, err)
	}
	for _, m := range groups[0].Members {
		if m.Kind == "proxy" && m.Name == target.SourceNodeName && m.ProxyName != "改名后的节点" {
			t.Errorf("member view = %+v, want proxyName to follow the rename", m)
		}
	}
}
