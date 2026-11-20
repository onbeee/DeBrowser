package service

import "testing"

// SyncRuntimeInfo is the sync module's only window into the
// instance runtime: unknown instances report ok=false, stopped
// instances report not-running, and identity verification
// fails closed for anything not fully verified.
func TestSyncRuntimeInfoFailsClosed(t *testing.T) {
	svc, cfg := testInstanceService(t)

	if _, ok := svc.SyncRuntimeInfo("no-such-instance"); ok {
		t.Fatal("unknown instance must report ok=false")
	}

	inst, err := svc.CreateInstance("", CreateInstanceInput{Name: "同步身份测试", BrowserID: testBrowserID(t, cfg)})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	info, ok := svc.SyncRuntimeInfo(inst.ID)
	if !ok {
		t.Fatal("existing instance must report ok=true")
	}
	if info.Running || info.Verified || info.CDPPort != 0 {
		t.Fatalf("stopped instance must be unverified with no port: %+v", info)
	}
	if svc.VerifySyncIdentity(info) {
		t.Fatal("unverified info must never verify")
	}
	forged := SyncRuntimeInfo{
		InstanceID: inst.ID, Running: true, Verified: true,
		PID: 424242, ProcessCreateAt: 1, ExecutablePath: "x",
	}
	if svc.VerifySyncIdentity(forged) {
		t.Fatal("forged identity (no tracked process) must not verify")
	}
}

// StartSync input validation happens before any connection
// work: no master, master-as-follower and empty followers are
// all rejected, and a second session cannot start over a live
// one.
func TestStartSyncValidation(t *testing.T) {
	svc, _ := testInstanceService(t)
	sync := NewSyncService(svc)

	if _, err := sync.StartSync("", "", []string{"a"}); err == nil {
		t.Fatal("empty master must be rejected")
	}
	if _, err := sync.StartSync("", "a", nil); err == nil {
		t.Fatal("empty followers must be rejected")
	}
	if _, err := sync.StartSync("", "a", []string{"a"}); err == nil {
		t.Fatal("master as its own follower must be rejected")
	}
	if _, err := sync.StartSync("", "ghost-master", []string{"ghost-follower"}); err == nil {
		t.Fatal("non-running master must be rejected")
	}
	if st, err := sync.GetSyncState(""); err != nil || st.Phase != "已停止" {
		t.Fatalf("state after failed starts = %+v, %v", st, err)
	}
}
