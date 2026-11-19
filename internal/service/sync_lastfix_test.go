package service

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// Last fix round for PR #20: (1) a master target must produce
// AT MOST ONE follower page per follower, no matter whether the
// TargetCreated callback or a visibility-driven refresh asks
// first, asks concurrently, or asks repeatedly while creation is
// still in flight; (2) creation failure must leave no mapping
// and no in-flight creation state behind.

// waitMapped polls until the master target is mapped on the
// follower and returns the follower target.
func waitMapped(t *testing.T, worker *followerWorker, masterTarget string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ft, ok := worker.mappedTarget(masterTarget); ok {
			return ft
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("master target %s was never mapped on the follower", masterTarget)
	return ""
}

// creatingCount reports the session's in-flight creation count.
func creatingCount(sess *syncSession) int {
	sess.createMu.Lock()
	defer sess.createMu.Unlock()
	return len(sess.creating)
}

// switchMasterToM2 makes the master's pre-existing second tab
// the visible one, which drives refreshMasterActive ->
// pointFollower through the real visibility path.
func switchMasterToM2(master *fakeChromium) {
	master.PushBindingEvent("M1", PageEvent{Kind: "visibility", State: "hidden", Href: "https://a.example/"})
	master.PushBindingEvent("M2", PageEvent{Kind: "visibility", State: "visible", Href: "https://b.example/"})
}

func TestNewTabVisibilityFirstCreatesOneFollowerPage(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	// Creation is slow: while the visibility-driven creation is
	// still in flight, the TargetCreated callback for the same
	// master target arrives late, and another refresh runs.
	follower.setCreateDelay(300 * time.Millisecond)
	switchMasterToM2(master)
	time.Sleep(50 * time.Millisecond) // creation is in flight now
	sess.onMasterTargetCreated("M2")
	go sess.pointFollower(worker, "M2", "https://b.example/")

	waitMapped(t, worker, "M2")
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("follower received %d Target.createTarget commands, want exactly 1", got)
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("in-flight creation entries left behind: %d", n)
	}
}

func TestNewTabTargetCreatedFirstCreatesOneFollowerPage(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	// The TargetCreated callback starts creation first; the
	// user switches to the new tab immediately afterwards, so
	// the visibility-driven refresh asks for the same page
	// while creation is still in flight.
	follower.setCreateDelay(300 * time.Millisecond)
	sess.onMasterTargetCreated("M2")
	time.Sleep(50 * time.Millisecond) // creation is in flight now
	switchMasterToM2(master)

	waitMapped(t, worker, "M2")
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("follower received %d Target.createTarget commands, want exactly 1", got)
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("in-flight creation entries left behind: %d", n)
	}
}

func TestNewTabConcurrentTriggersCreateOneFollowerPage(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	follower.setCreateDelay(500 * time.Millisecond)
	switchMasterToM2(master) // the visibility path fires one trigger itself
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(3)
		go func() { defer wg.Done(); sess.onMasterTargetCreated("M2") }()
		go func() { defer wg.Done(); sess.pointFollower(worker, "M2", "https://b.example/") }()
		go func() { defer wg.Done(); sess.refreshMasterActive() }()
	}
	waitMapped(t, worker, "M2")
	wg.Wait()
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("concurrent triggers produced %d Target.createTarget commands, want exactly 1", got)
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("in-flight creation entries left behind: %d", n)
	}
}

func TestNewTabRepeatedTriggersDuringCreationCreateOne(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	// Creation takes ~600ms; triggers keep arriving throughout.
	follower.setCreateDelay(600 * time.Millisecond)
	sess.onMasterTargetCreated("M2")
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess.onMasterTargetCreated("M2")
			sess.pointFollower(worker, "M2", "https://b.example/")
		}()
		time.Sleep(60 * time.Millisecond)
	}
	waitMapped(t, worker, "M2")
	wg.Wait()
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("repeated triggers during creation produced %d Target.createTarget commands, want exactly 1", got)
	}
}

func TestNewTabCreationFailureCleansStateAndAllowsRetry(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	follower.setFailCreate(true)
	sess.pointFollower(worker, "M2", "https://b.example/")
	if _, ok := worker.mappedTarget("M2"); ok {
		t.Fatal("a failed creation must not leave a mapping behind")
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("a failed creation left %d in-flight entries behind", n)
	}
	if state, reason, _ := worker.snapshot(); state != followerUnconfirmed || !strings.Contains(reason, "创建对应页面") {
		t.Fatalf("follower after failed creation = %q / %q, want unconfirmed with the creation failure recorded", state, reason)
	}
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("failed attempt used %d Target.createTarget commands, want 1", got)
	}

	// The in-flight state was cleaned up, so a later trigger
	// retries cleanly and succeeds — exactly one more creation.
	follower.setFailCreate(false)
	sess.pointFollower(worker, "M2", "https://b.example/")
	waitMapped(t, worker, "M2")
	if got := follower.createTargetCount(); got != 2 {
		t.Fatalf("after retry the follower received %d Target.createTarget commands, want 2 (one failed, one succeeded)", got)
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("in-flight creation entries left behind after retry: %d", n)
	}
}

func TestNewTabReadinessFailureLeavesNoMappingOrCreationState(t *testing.T) {
	master := newFakeChromium(t, map[string]string{
		"M1": "https://a.example/", "M2": "https://b.example/",
	}, "M1")
	follower := newFakeChromium(t, map[string]string{"F1": "https://a.example/"}, "F1")
	sess, worker := startTestSession(t, master, follower, "M1", "F1")

	// Creation succeeds (the fake's first created target is
	// created-1) but attaching to it fails: no mapping may be
	// established and no creation state may remain.
	follower.failAttachFor("created-1")
	sess.pointFollower(worker, "M2", "https://b.example/")
	if _, ok := worker.mappedTarget("M2"); ok {
		t.Fatal("a page that never became ready must not be mapped")
	}
	if n := creatingCount(sess); n != 0 {
		t.Fatalf("a readiness failure left %d in-flight entries behind", n)
	}
	if state, reason, _ := worker.snapshot(); state != followerFailed || !strings.Contains(reason, "接入失败") {
		t.Fatalf("follower after readiness failure = %q / %q, want failed with the attach failure recorded", state, reason)
	}
	if got := follower.createTargetCount(); got != 1 {
		t.Fatalf("readiness-failure attempt used %d Target.createTarget commands, want 1", got)
	}
}
