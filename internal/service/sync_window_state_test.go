package service

import (
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// Regression tests for the Stage 10 window-state fixes:
// mirror baseline seeding, layout/mirror serialization
// effects, visible-target distinctions, shared phase labels
// and honest resume results.

func setVisible(t *pageTracker, states map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, v := range states {
		t.visible[id] = v
	}
}

func TestVisibleStateDistinguishesNoneOneMany(t *testing.T) {
	track := newPageTracker(nil, true, nil, nil)
	if id, n := track.visibleState(); n != 0 || id != "" {
		t.Fatalf("no reports: got (%q, %d), want (\"\", 0)", id, n)
	}
	if got := track.VisibleTarget(); got != "" {
		t.Fatalf("no reports: VisibleTarget = %q, want \"\"", got)
	}
	setVisible(track, map[string]bool{"A": true, "B": false})
	if id, n := track.visibleState(); n != 1 || id != "A" {
		t.Fatalf("one visible: got (%q, %d), want (\"A\", 1)", id, n)
	}
	setVisible(track, map[string]bool{"B": true})
	if id, n := track.visibleState(); n != 2 || id != "" {
		t.Fatalf("two visible: got (%q, %d), want (\"\", 2) — never guessed", id, n)
	}
	if got := track.VisibleTarget(); got != "" {
		t.Fatalf("two visible: VisibleTarget = %q, want \"\"", got)
	}
	// A destroyed target drops out of the visibility set.
	track.Forget("B")
	if id, n := track.visibleState(); n != 1 || id != "A" {
		t.Fatalf("after destroy: got (%q, %d), want (\"A\", 1)", id, n)
	}
}

func TestRefreshActiveKeepsTargetThroughTransientInvisibility(t *testing.T) {
	track := newPageTracker(nil, true, nil, nil)
	sess := &syncSession{
		phase:        syncRunning,
		systemPauses: map[string]bool{},
		followers:    map[string]*followerWorker{},
		masterTrack:  track,
		activeTarget: "T1",
	}
	// Master window covered: no visible target. The session
	// pauses, but the confirmed active target is NOT lost.
	sess.refreshActive()
	sess.mu.Lock()
	paused := sess.systemPauses["target"]
	active := sess.activeTarget
	sess.mu.Unlock()
	if !paused {
		t.Fatal("no visible target must pause the session")
	}
	if active != "T1" {
		t.Fatalf("transient invisibility discarded the active target: %q", active)
	}
	// Two visible targets: still paused, still no guessing.
	setVisible(track, map[string]bool{"T1": true, "T2": true})
	sess.refreshActive()
	sess.mu.Lock()
	paused = sess.systemPauses["target"]
	active = sess.activeTarget
	sess.mu.Unlock()
	if !paused || active != "T1" {
		t.Fatalf("multiple visible: paused=%v active=%q, want paused with T1 kept", paused, active)
	}
	// Back to exactly one visible target: pause clears.
	setVisible(track, map[string]bool{"T2": false})
	sess.refreshActive()
	sess.mu.Lock()
	paused = sess.systemPauses["target"]
	sess.mu.Unlock()
	if paused {
		t.Fatal("a unique visible target must clear the target pause")
	}
}

func TestMirrorBaselineSeedsThenDetectsChanges(t *testing.T) {
	sess := &syncSession{}
	pl := winmgr.Placement{ShowCmd: 1, NormalRect: winmgr.Rect{X: 10, Y: 10, Width: 800, Height: 600}}
	scr := winmgr.Rect{X: 10, Y: 10, Width: 800, Height: 600}
	// First observation ESTABLISHES the baseline; it is not
	// a change (this is what stacked every window at start).
	if sess.noteMasterGeometry(pl, scr) {
		t.Fatal("first geometry observation must seed the baseline, not report a change")
	}
	if sess.noteMasterGeometry(pl, scr) {
		t.Fatal("unchanged geometry must not report a change")
	}
	moved := winmgr.Rect{X: 300, Y: 10, Width: 800, Height: 600}
	if !sess.noteMasterGeometry(pl, moved) {
		t.Fatal("a real master move must report a change")
	}
	if sess.noteMasterGeometry(pl, moved) {
		t.Fatal("the new geometry is now the baseline")
	}
	// A fresh session starts with NO baseline again: no
	// stale geometry survives stop/start.
	fresh := &syncSession{}
	if fresh.noteMasterGeometry(pl, scr) {
		t.Fatal("a new session must seed its baseline, not replay the old one")
	}
}

func TestMirrorOnceSkipsUnlessLive(t *testing.T) {
	svc := &SyncService{}
	pl := winmgr.Placement{ShowCmd: 1}
	scr := winmgr.Rect{Width: 100, Height: 100}
	cases := map[string]*syncSession{
		"manual pause": {phase: syncRunning, manualPaused: true, systemPauses: map[string]bool{}},
		"system pause": {phase: syncRunning, systemPauses: map[string]bool{"target": true}},
		"starting":     {phase: syncStarting, systemPauses: map[string]bool{}},
		"stopping":     {phase: syncStopping, systemPauses: map[string]bool{}},
		"torn down":    {phase: syncRunning, tornDown: true, systemPauses: map[string]bool{}},
	}
	for name, sess := range cases {
		sess.masterHWND = 777
		svc.mirrorOnce(sess) // must return before any window/instance access
		if sess.lastMasterPl != nil || sess.lastMasterScr != nil {
			t.Fatalf("%s: mirror touched the baseline while not live", name)
		}
		_ = pl
		_ = scr
	}
}

func TestRestoreRefreshesMirrorBaseline(t *testing.T) {
	hwnd := uintptr(555)
	stale := winmgr.Placement{ShowCmd: 1, NormalRect: winmgr.Rect{X: 1, Y: 1, Width: 10, Height: 10}}
	staleScr := winmgr.Rect{X: 1, Y: 1, Width: 10, Height: 10}
	fresh := winmgr.Placement{ShowCmd: 1, NormalRect: winmgr.Rect{X: 40, Y: 50, Width: 900, Height: 700}}
	freshScr := winmgr.Rect{X: 40, Y: 50, Width: 900, Height: 700}
	sess := &syncSession{
		phase:         syncRunning,
		systemPauses:  map[string]bool{},
		followers:     map[string]*followerWorker{},
		masterHWND:    hwnd,
		lastMasterPl:  &stale,
		lastMasterScr: &staleScr,
	}
	sess.master = SyncRuntimeInfo{InstanceID: "m", PID: 4242}
	svc := &SyncService{
		sess:            sess,
		savedPlacements: map[uintptr]savedWindow{hwnd: {InstanceID: "m", PID: 4242, CreatedAt: 1, Placement: fresh}},
	}
	svc.verifySavedWindow = func(uintptr, savedWindow) bool { return true }
	svc.setWindowPlacement = func(uintptr, winmgr.Placement) error { return nil }
	svc.windowOwnerPID = func(uintptr) int { return 4242 }
	svc.getWindowGeometry = func(uintptr) (winmgr.Placement, winmgr.Rect, error) {
		return fresh, freshScr, nil
	}
	if err := svc.restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.lastMasterPl == nil || *sess.lastMasterPl != fresh || sess.lastMasterScr == nil || *sess.lastMasterScr != freshScr {
		t.Fatal("restore must refresh the mirror baseline to the restored master geometry")
	}
}

func TestStateViewSharesOnePhase(t *testing.T) {
	instances, _ := testInstanceService(t)
	svc := NewSyncService(instances)
	sess := &syncSession{
		id:              "s1",
		masterID:        "m",
		phase:           syncRunning,
		systemPauses:    map[string]bool{"target": true},
		followers:       map[string]*followerWorker{"ok": {}, "bad": nil},
		failedFollowers: map[string]string{"bad": "启动失败原因"},
	}
	svc.sess = sess
	view := svc.state()
	if view.Phase != "已暂停" {
		t.Fatalf("phase = %q, want 已暂停", view.Phase)
	}
	byRole := map[string]SyncParticipantView{}
	for _, p := range view.Participants {
		byRole[p.Role+"/"+p.InstanceID] = p
	}
	if got := byRole["master/m"].Status; got != "已暂停" {
		t.Fatalf("master status = %q, want 已暂停 (same phase as followers)", got)
	}
	if got := byRole["follower/ok"].Status; got != "已暂停" {
		t.Fatalf("healthy follower status = %q, want 已暂停", got)
	}
	bad := byRole["follower/bad"]
	if bad.Status != "失败" || bad.Reason != "启动失败原因" {
		t.Fatalf("failed follower must keep its own status/reason, got %+v", bad)
	}
	// Starting: master and healthy follower both show 启动中.
	sess.mu.Lock()
	sess.phase = syncStarting
	sess.systemPauses = map[string]bool{}
	sess.mu.Unlock()
	view = svc.state()
	for _, p := range view.Participants {
		if p.InstanceID == "bad" {
			continue
		}
		if p.Status != "启动中" {
			t.Fatalf("starting: %s status = %q, want 启动中", p.InstanceID, p.Status)
		}
	}
}

func TestPauseReasonText(t *testing.T) {
	base := func() *syncSession {
		return &syncSession{phase: syncRunning, systemPauses: map[string]bool{}, followers: map[string]*followerWorker{}}
	}
	if got := base().pauseReasonText(); got != "" {
		t.Fatalf("running unpaused: %q, want \"\"", got)
	}
	s := base()
	s.systemPauses["target"] = true
	if got := s.pauseReasonText(); !strings.Contains(got, "无法确认主实例当前显示的标签页") {
		t.Fatalf("target pause reason: %q", got)
	}
	s = base()
	s.systemPauses["dialog-follower"] = true
	if got := s.pauseReasonText(); !strings.Contains(got, "跟随实例有未处理的对话框") {
		t.Fatalf("dialog pause reason: %q", got)
	}
	s = base()
	s.manualPaused = true
	if got := s.pauseReasonText(); got == "" {
		t.Fatal("manual pause must produce a reason")
	}
	s = base()
	s.phase = syncStarting
	if got := s.pauseReasonText(); got == "" {
		t.Fatal("a non-running phase must produce a reason")
	}
}

func TestResumeSyncFailsWhenMasterIdentityGone(t *testing.T) {
	instances, _ := testInstanceService(t)
	svc := NewSyncService(instances)
	sess := &syncSession{
		id:           "s1",
		masterID:     "ghost",
		phase:        syncRunning,
		systemPauses: map[string]bool{},
		followers:    map[string]*followerWorker{},
	}
	svc.sess = sess
	if _, err := svc.ResumeSync(""); err == nil {
		t.Fatal("resume must fail when the master identity cannot be verified — never a fake success")
	}
	if svc.current() != nil {
		t.Fatal("the session must be torn down after a failed resume identity check")
	}
}
