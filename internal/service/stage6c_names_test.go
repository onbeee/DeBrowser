package service

import (
	"context"
	"strings"
	"testing"
)

// Name-conflict rules (Stage 6C final fix): inside ONE config's
// effective node set — its imported nodes plus the distinct
// custom nodes attached through the overlay — two different
// nodes may not share a name, because Mihomo groups reference
// members by name. These tests drive the real service entry
// points: AddCustomGroupMember, UpdateProxy (rename) and
// RefreshSubscription / ImportLocalConfig (applyImport).

const twoGroupFeed = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a}
proxy-groups:
  - {name: 组一, type: select, proxies: [节点甲]}
  - {name: 组二, type: select, proxies: [节点甲, DIRECT]}
`

// conflictFeed imports a node named 特别节点 — used to collide
// with an attached custom node of the same name.
const conflictFeed = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a}
  - {name: 特别节点, type: ss, server: z.example.com, port: 8388, cipher: aes-256-gcm, password: pw-z}
proxy-groups:
  - {name: 选择组, type: select, proxies: [节点甲]}
`

func rename(t *testing.T, f subFixture, id, newName string) error {
	t.Helper()
	cur, err := f.proxy.GetProxy(id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.proxy.UpdateProxy(UpdateProxyInput{
		ID: id, Name: newName, Protocol: cur.Protocol, Address: cur.Address,
		Port: cur.Port, ConfigJSON: cur.ConfigJSON, Enabled: cur.Enabled,
	})
	return err
}

func overlayCount(t *testing.T, f subFixture, proxyID string) int {
	t.Helper()
	rows, err := f.store.ListCustomMembershipsByProxy(context.Background(), proxyID)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestNameConflictOnAdd(t *testing.T) {
	f := newSubFixture(t)
	_, subConfigID := seedSubWithGroup(t, f)
	fileConfigID := importFileFix(t, f, fileFeedFix, "本地四")

	// 1. Custom node named like an imported node of the config.
	clash := createCustomNode(t, f, "节点甲")
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", clash.ID); err == nil {
		t.Error("add with imported-name clash was accepted")
	}
	// 6. The rejection left no overlay row.
	if n := overlayCount(t, f, clash.ID); n != 0 {
		t.Errorf("overlay rows after rejected add = %d, want 0", n)
	}

	// 2. Two different custom nodes with the SAME name: the first
	// joins the file config, the second is refused even into a
	// DIFFERENT group of the same config.
	dupA := createCustomNode(t, f, "同名节点")
	dupB := createCustomNode(t, f, "同名节点")
	if err := f.svc.AddCustomGroupMember(fileConfigID, "自动选择", dupA.ID); err != nil {
		t.Fatalf("first same-name add: %v", err)
	}
	if err := f.svc.AddCustomGroupMember(fileConfigID, "手动切换", dupB.ID); err == nil {
		t.Error("second same-name node was accepted into the same config")
	}
	if n := overlayCount(t, f, dupB.ID); n != 0 {
		t.Errorf("overlay rows for refused node = %d, want 0", n)
	}

	// 3. The SAME node joining several groups of one config is
	// fine (it is one node, referenced twice).
	if err := f.svc.AddCustomGroupMember(fileConfigID, "手动切换", dupA.ID); err != nil {
		t.Errorf("same node into a second group of the config: %v", err)
	}

	// 4. Same-named custom nodes in DIFFERENT configs are fine:
	// dupB (still unattached) joins the subscription config,
	// whose effective set has no 同名节点 yet.
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", dupB.ID); err != nil {
		t.Errorf("same name in a different config should be allowed: %v", err)
	}

	// 5. Distinct names join normally.
	other := createCustomNode(t, f, "另一个节点")
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", other.ID); err != nil {
		t.Errorf("distinct-name add: %v", err)
	}
}

func TestNameConflictOnRename(t *testing.T) {
	f := newSubFixture(t)
	_, subConfigID := seedSubWithGroup(t, f)
	fileConfigID := importFileFix(t, f, fileFeedFix, "本地五")

	// 7. Unattached node renames freely.
	free := createCustomNode(t, f, "自由节点")
	if err := rename(t, f, free.ID, "改名一"); err != nil {
		t.Errorf("unattached rename: %v", err)
	}

	node := createCustomNode(t, f, "被测节点")
	peer := createCustomNode(t, f, "同伴节点")
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", node.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", peer.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddCustomGroupMember(fileConfigID, "自动选择", node.ID); err != nil {
		t.Fatal(err)
	}
	refBefore := customSecretRef(t, f, node.ID)

	// 8. Rename colliding with an imported node of an attached
	// config is refused; name and vault ref unchanged (13).
	if err := rename(t, f, node.ID, "节点甲"); err == nil {
		t.Error("rename into an imported name was accepted")
	}
	// 9. Rename colliding with another attached custom node of
	// the same config is refused.
	if err := rename(t, f, node.ID, "同伴节点"); err == nil {
		t.Error("rename into a peer custom name was accepted")
	}
	// 10. Rename colliding only in the SECOND attached config
	// （文件甲 is imported there) refuses the WHOLE rename.
	if err := rename(t, f, node.ID, "文件甲"); err == nil {
		t.Error("rename conflicting in one of several configs was accepted")
	}
	cur, err := f.proxy.GetProxy(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Name != "被测节点" {
		t.Errorf("name after refused renames = %q, want 被测节点", cur.Name)
	}
	if ref := customSecretRef(t, f, node.ID); ref != refBefore {
		t.Errorf("vault ref changed by refused renames: %q -> %q", refBefore, ref)
	}
	if n := overlayCount(t, f, node.ID); n != 2 {
		t.Errorf("memberships after refused renames = %d, want 2", n)
	}

	// A name that only exists in an UNRELATED config (peer is not
	// attached there) does not block: rename node (attached to
	// sub+file) — 同伴节点 is only in sub, already covered by 9;
	// use the file-config-only peer name instead: create one
	// attached only to the file config.
	filePeer := createCustomNode(t, f, "仅文件节点")
	if err := f.svc.AddCustomGroupMember(fileConfigID, "手动切换", filePeer.ID); err != nil {
		t.Fatal(err)
	}
	// node IS attached to the file config too, so this must clash.
	if err := rename(t, f, node.ID, "仅文件节点"); err == nil {
		t.Error("rename into a file-config peer name was accepted")
	}
	// But the unattached node may take that name freely (19).
	if err := rename(t, f, free.ID, "仅文件节点"); err != nil {
		t.Errorf("unattached rename to a name used in another config: %v", err)
	}

	// 11 + 12. A rename free in every attached config succeeds,
	// and memberships still bind by proxy_id with the new name
	// displayed.
	if err := rename(t, f, node.ID, "全新名称"); err != nil {
		t.Fatalf("clean multi-config rename: %v", err)
	}
	if n := overlayCount(t, f, node.ID); n != 2 {
		t.Errorf("memberships after rename = %d, want 2", n)
	}
	groups, err := f.svc.GetConfigGroups(subConfigID)
	if err != nil {
		t.Fatal(err)
	}
	shown := false
	for _, g := range groups {
		for _, m := range g.Members {
			if m.Origin == "custom" && m.ProxyID == node.ID && m.ProxyName == "全新名称" {
				shown = true
			}
		}
	}
	if !shown {
		t.Error("renamed node not displayed under its new name in the group view")
	}
}

func TestNameConflictOnRefreshAndImport(t *testing.T) {
	f := newSubFixture(t)
	subID, subConfigID := seedSubWithGroup(t, f)

	special := createCustomNode(t, f, "特别节点")
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", special.ID); err != nil {
		t.Fatal(err)
	}
	refBefore := customSecretRef(t, f, special.ID)

	// Snapshot of the pre-refresh state.
	groupsBefore, err := f.svc.GetConfigGroups(subConfigID)
	if err != nil {
		t.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	proxiesBefore, err := f.store.ListProxiesBySubscription(c, subID)
	if err != nil {
		t.Fatal(err)
	}

	// 14. A refresh importing a node with the attached custom
	// node's name fails.
	f.stubFeed(conflictFeed)
	if _, err := f.svc.RefreshSubscription(subID); err == nil {
		t.Fatal("conflicting refresh was accepted")
	} else if !strings.Contains(err.Error(), "特别节点") {
		t.Errorf("refresh error should name the conflict: %v", err)
	}

	// 15. Everything from the last good import is intact.
	groupsAfter, err := f.svc.GetConfigGroups(subConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(groupsAfter) != len(groupsBefore) || len(groupsAfter[0].Members) != len(groupsBefore[0].Members) {
		t.Errorf("group view changed after failed refresh: %+v -> %+v", groupsBefore, groupsAfter)
	}
	proxiesAfter, err := f.store.ListProxiesBySubscription(c, subID)
	if err != nil {
		t.Fatal(err)
	}
	if len(proxiesAfter) != len(proxiesBefore) {
		t.Fatalf("proxy count changed after failed refresh: %d -> %d", len(proxiesBefore), len(proxiesAfter))
	}
	for i := range proxiesBefore {
		if proxiesAfter[i].ID != proxiesBefore[i].ID || proxiesAfter[i].SecretRef != proxiesBefore[i].SecretRef {
			t.Errorf("proxy %d changed after failed refresh", i)
		}
	}
	if n := overlayCount(t, f, special.ID); n != 1 {
		t.Errorf("overlay rows after failed refresh = %d, want 1", n)
	}
	if ref := customSecretRef(t, f, special.ID); ref != refBefore {
		t.Error("custom node vault ref changed by failed refresh")
	}

	// 16. A non-conflicting refresh keeps the overlay as before.
	f.stubFeed(subFeedV2)
	if _, err := f.svc.RefreshSubscription(subID); err != nil {
		t.Fatalf("clean refresh: %v", err)
	}
	if n := overlayCount(t, f, special.ID); n != 1 {
		t.Errorf("overlay rows after clean refresh = %d, want 1", n)
	}

	// 17. Local import path: the same guard runs inside
	// applyImport. A fresh local config has no overlay, and a
	// name that is attached in ANOTHER config must not block the
	// import (no cross-config interference, 19).
	fileConfigID := importFileFix(t, f, fileFeedFix, "本地六")
	if fileConfigID == "" {
		t.Fatal("local import returned no config")
	}
	// Attaching the special node to the file config too, then
	// importing ANOTHER local file whose document contains a
	// node named 特别节点 — still a fresh config, still fine.
	conflictFile := "proxies:\n  - {name: 特别节点, type: ss, server: q.example.com, port: 8388, cipher: aes-256-gcm, password: pw-q}\n"
	if id := importFileFix(t, f, conflictFile, "本地七"); id == "" {
		t.Error("local import blocked by a same-named custom node of another config")
	}

	// 18. The same custom node in TWO groups of one config is
	// counted once: set up a two-group subscription, attach the
	// node to both groups, then a conflicting refresh still
	// fails (single conflict), and a clean refresh keeps both
	// memberships.
	f2 := newSubFixture(t)
	sub2 := f2.createSub(t)
	f2.stubFeed(twoGroupFeed)
	if _, err := f2.svc.RefreshSubscription(sub2.ID); err != nil {
		t.Fatal(err)
	}
	cfg2 := mustConfigID(t, f2, sub2.ID)
	multi := createCustomNode(t, f2, "多组节点")
	clash := createCustomNode(t, f2, "撞名节点")
	if err := f2.svc.AddCustomGroupMember(cfg2, "组一", multi.ID); err != nil {
		t.Fatal(err)
	}
	if err := f2.svc.AddCustomGroupMember(cfg2, "组二", multi.ID); err != nil {
		t.Fatal(err)
	}
	if err := f2.svc.AddCustomGroupMember(cfg2, "组一", clash.ID); err != nil {
		t.Fatal(err)
	}
	// A well-formed document whose imported node (and the group
	// references to it) takes the attached custom node's name —
	// parseable, so only the name guard can refuse it.
	clashFeed := strings.ReplaceAll(twoGroupFeed, "节点甲", "撞名节点")
	f2.stubFeed(clashFeed)
	if _, err := f2.svc.RefreshSubscription(sub2.ID); err == nil {
		t.Error("refresh conflicting with a multi-group member was accepted")
	}
	f2.stubFeed(twoGroupFeed)
	if _, err := f2.svc.RefreshSubscription(sub2.ID); err != nil {
		t.Fatalf("clean refresh after conflict: %v", err)
	}
	if n := overlayCount(t, f2, multi.ID); n != 2 {
		t.Errorf("multi-group memberships after refreshes = %d, want 2", n)
	}
}
