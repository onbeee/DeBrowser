package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fileFeedFix is a local-file document whose groups differ from
// the subscription feed's, for multi-config overlay tests.
const fileFeedFix = `proxies:
  - {name: 文件甲, type: ss, server: f.example.com, port: 8388, cipher: aes-256-gcm, password: pw-f}
proxy-groups:
  - {name: 自动选择, type: url-test, url: "https://example.com", interval: 300, proxies: [文件甲]}
  - {name: 手动切换, type: select, proxies: [文件甲, DIRECT]}
`

const noGroupFeed = `proxies:
  - {name: 节点甲, type: ss, server: a.example.com, port: 8388, cipher: aes-256-gcm, password: pw-a}
`

func importFileFix(t *testing.T, f subFixture, content, name string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.FilePicker = func() (string, error) { return file, nil }
	sum, err := f.svc.ImportLocalConfig(name)
	if err != nil {
		t.Fatalf("ImportLocalConfig: %v", err)
	}
	return sum.ConfigID
}

func seedSubWithGroup(t *testing.T, f subFixture) (subID, configID string) {
	t.Helper()
	sub := f.createSub(t)
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(sub.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	return sub.ID, mustConfigID(t, f, sub.ID)
}

func createCustomNode(t *testing.T, f subFixture, name string) ProxyView {
	t.Helper()
	port := 1080
	v, err := f.proxy.CreateProxy(CreateProxyInput{
		Name: name, Protocol: "socks5", Address: "127.0.0.1", Port: &port,
		Secrets: map[string]string{"password": "pw-" + name},
	})
	if err != nil {
		t.Fatalf("CreateProxy(%s): %v", name, err)
	}
	return v
}

// TestCustomGroupMemberLifecycle covers the review-fix data
// tests: add, multi-group/multi-config reuse, duplicate refusal,
// non-custom refusal, missing config/group/node refusal, removal
// keeping the node, node deletion cleaning the overlay, config
// deletion cleaning the overlay, and vault references untouched
// throughout.
func TestCustomGroupMemberLifecycle(t *testing.T) {
	f := newSubFixture(t)
	_, subConfigID := seedSubWithGroup(t, f)
	fileConfigID := importFileFix(t, f, fileFeedFix, "本地二")

	custom1 := createCustomNode(t, f, "自建一")
	custom2 := createCustomNode(t, f, "自建二")

	// 1. Add to one group.
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", custom1.ID); err != nil {
		t.Fatalf("add custom member: %v", err)
	}
	// 2. The same node joins groups of another config too.
	if err := f.svc.AddCustomGroupMember(fileConfigID, "自动选择", custom1.ID); err != nil {
		t.Fatalf("add to second config: %v", err)
	}
	if err := f.svc.AddCustomGroupMember(fileConfigID, "手动切换", custom1.ID); err != nil {
		t.Fatalf("add to second group: %v", err)
	}
	// 3. Duplicate in the same group is refused.
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", custom1.ID); err == nil {
		t.Error("duplicate add was accepted")
	}
	// 4. An imported (subscription) node is not a custom node.
	all, err := f.proxy.ListProxies()
	if err != nil {
		t.Fatal(err)
	}
	var importedID string
	for _, v := range all {
		if v.Source == "subscription" {
			importedID = v.ID
		}
	}
	if importedID == "" {
		t.Fatal("no imported node in fixture")
	}
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", importedID); err == nil {
		t.Error("imported node was accepted as a custom member")
	}
	// 5. Missing config / group / node are refused.
	if err := f.svc.AddCustomGroupMember("不存在的配置", "选择组", custom2.ID); err == nil {
		t.Error("add to missing config was accepted")
	}
	if err := f.svc.AddCustomGroupMember(subConfigID, "不存在的组", custom2.ID); err == nil {
		t.Error("add to missing group was accepted")
	}
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", "不存在的节点"); err == nil {
		t.Error("add of missing node was accepted")
	}

	// The merged group view: imported members first (origin
	// imported), the custom member appended (origin custom).
	// subFeedV1's 选择组 = [节点甲, 节点乙, DIRECT] => 3 imported.
	groups, err := f.svc.GetConfigGroups(subConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || len(groups[0].Members) != 4 {
		t.Fatalf("merged group view = %+v", groups)
	}
	for i := 0; i < 3; i++ {
		if groups[0].Members[i].Origin != "imported" {
			t.Errorf("member %d origin = %q, want imported", i, groups[0].Members[i].Origin)
		}
	}
	last := groups[0].Members[3]
	if last.Origin != "custom" || last.ProxyID != custom1.ID {
		t.Errorf("appended member = %+v, want the custom node", last)
	}
	// Candidates exclude the attached node, include the other.
	cands, err := f.svc.ListCustomCandidates(subConfigID, "选择组")
	if err != nil {
		t.Fatal(err)
	}
	foundC2 := false
	for _, cv := range cands {
		if cv.ID == custom1.ID {
			t.Error("attached node still offered as candidate")
		}
		if cv.ID == custom2.ID {
			foundC2 = true
		}
	}
	if !foundC2 {
		t.Error("unattached custom node missing from candidates")
	}
	// Memberships of the reused node: three rows, groups exist.
	ms, err := f.svc.ListCustomMemberships(custom1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 {
		t.Fatalf("memberships = %d, want 3: %+v", len(ms), ms)
	}
	for _, m := range ms {
		if !m.GroupExists || m.ConfigName == "" {
			t.Errorf("membership view incomplete: %+v", m)
		}
	}

	// The node's own vault credentials are untouched.
	ref := customSecretRef(t, f, custom1.ID)
	if ref == "" || !f.vaultHas(ref) {
		t.Fatal("custom node vault envelope missing after overlay ops")
	}

	// 6. Removing the membership keeps the node (and credentials).
	if err := f.svc.RemoveCustomGroupMember(fileConfigID, "手动切换", custom1.ID); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if _, err := f.proxy.GetProxy(custom1.ID); err != nil {
		t.Fatalf("node vanished after membership removal: %v", err)
	}
	if !f.vaultHas(ref) {
		t.Error("vault envelope removed together with the membership")
	}
	if err := f.svc.RemoveCustomGroupMember(fileConfigID, "手动切换", custom1.ID); err == nil {
		t.Error("removing a missing membership was accepted")
	}

	// 7. Deleting the node cleans its memberships.
	if err := f.proxy.DeleteProxy(custom1.ID); err != nil {
		t.Fatal(err)
	}
	c, cancel := ctx()
	defer cancel()
	rows, err := f.store.ListCustomMembershipsByProxy(c, custom1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("memberships survived node deletion: %+v", rows)
	}

	// 8. Deleting a config cleans its overlay rows only.
	custom3 := createCustomNode(t, f, "自建三")
	if err := f.svc.AddCustomGroupMember(fileConfigID, "自动选择", custom3.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteConfig(fileConfigID); err != nil {
		t.Fatal(err)
	}
	rows, err = f.store.ListCustomMembershipsByProxy(c, custom3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("memberships survived config deletion: %+v", rows)
	}
	if _, err := f.proxy.GetProxy(custom3.ID); err != nil {
		t.Errorf("custom node was deleted together with the config: %v", err)
	}
}

// TestCustomMembersSurviveSubscriptionRefresh walks the
// refresh-compatibility sequence: group present → member stays;
// group removed upstream → relationship kept but reported as
// orphaned; group returns → member reappears. Imported members'
// order and stable IDs never change. The same rebuild primitive
// (ReplaceProxyGroups) is then exercised against a FILE config's
// overlay directly.
func TestCustomMembersSurviveSubscriptionRefresh(t *testing.T) {
	f := newSubFixture(t)
	subID, subConfigID := seedSubWithGroup(t, f)
	custom := createCustomNode(t, f, "常驻自建")
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", custom.ID); err != nil {
		t.Fatal(err)
	}
	importedShape := func() string {
		groups, err := f.svc.GetConfigGroups(subConfigID)
		if err != nil {
			t.Fatal(err)
		}
		var parts []string
		for _, g := range groups {
			for _, m := range g.Members {
				if m.Origin == "imported" {
					parts = append(parts, g.Name+"/"+m.Name+"/"+m.ProxyID)
				}
			}
		}
		return strings.Join(parts, "|")
	}
	memberPresent := func() bool {
		groups, err := f.svc.GetConfigGroups(subConfigID)
		if err != nil {
			t.Fatal(err)
		}
		for _, g := range groups {
			if g.Name != "选择组" {
				continue
			}
			for _, m := range g.Members {
				if m.Origin == "custom" && m.ProxyID == custom.ID {
					return true
				}
			}
		}
		return false
	}
	before := importedShape()
	if before == "" || !memberPresent() {
		t.Fatal("fixture broken: no imported members or custom member missing")
	}

	// Refresh with the same document: member + imported shape stay.
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(subID); err != nil {
		t.Fatal(err)
	}
	if !memberPresent() {
		t.Error("custom member lost after same-document refresh")
	}
	if got := importedShape(); got != before {
		t.Errorf("imported members changed across refresh:\n%s\n->\n%s", before, got)
	}

	// Refresh with a document WITHOUT the group: the relationship
	// is kept and reported orphaned, not silently deleted.
	f.stubFeed(noGroupFeed)
	if _, err := f.svc.RefreshSubscription(subID); err != nil {
		t.Fatal(err)
	}
	if memberPresent() {
		t.Error("custom member shown in a group that no longer exists")
	}
	c, cancel := ctx()
	defer cancel()
	rows, err := f.store.ListCustomMembershipsByProxy(c, custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("overlay rows after group removal = %d, want 1 (kept)", len(rows))
	}
	orphans, err := f.svc.ListOrphanedCustomMembers(subConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0].GroupName != "选择组" || len(orphans[0].Members) != 1 ||
		orphans[0].Members[0].ProxyID != custom.ID {
		t.Fatalf("orphan view = %+v", orphans)
	}
	ms, err := f.svc.ListCustomMemberships(custom.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].GroupExists {
		t.Errorf("membership view should mark the group missing: %+v", ms)
	}
	// Adding to a vanished group is refused.
	if err := f.svc.AddCustomGroupMember(subConfigID, "选择组", custom.ID); err == nil {
		t.Error("adding to a vanished group was accepted")
	}

	// Refresh with the group back: the member reappears.
	f.stubFeed(subFeedV1)
	if _, err := f.svc.RefreshSubscription(subID); err != nil {
		t.Fatal(err)
	}
	if !memberPresent() {
		t.Error("custom member did not reappear when the group returned")
	}
	if orphans, err := f.svc.ListOrphanedCustomMembers(subConfigID); err != nil || len(orphans) != 0 {
		t.Errorf("orphans after group return = %+v (err %v), want none", orphans, err)
	}

	// File config: the rebuild primitive every import path uses
	// (ReplaceProxyGroups) never touches the overlay table.
	fileConfigID := importFileFix(t, f, fileFeedFix, "本地三")
	if err := f.svc.AddCustomGroupMember(fileConfigID, "自动选择", custom.ID); err != nil {
		t.Fatal(err)
	}
	groupsNow, err := f.store.ListProxyGroups(c, fileConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.ReplaceProxyGroups(c, fileConfigID, groupsNow); err != nil {
		t.Fatal(err)
	}
	rows, err = f.store.ListCustomGroupMembers(c, fileConfigID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ProxyID != custom.ID {
		t.Errorf("file config overlay after group rebuild = %+v", rows)
	}

	// Credentials survived everything.
	if ref := customSecretRef(t, f, custom.ID); ref == "" || !f.vaultHas(ref) {
		t.Error("custom node vault envelope lost during the refresh cycle")
	}
}
