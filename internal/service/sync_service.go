package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// SyncService is the Stage 10 multi-instance synchronization
// service: one master instance's page interactions and browser
// operations are mirrored to follower instances, each fully
// isolated. Page operations travel over per-instance loopback
// CDP connections; window geometry travels over Win32 window
// APIs that never activate a window; the browser's native UI
// is only OBSERVED (low-level hooks + UIA classification) —
// effects that CDP confirms (a committed navigation) are what
// get synchronized, never simulated input.
//
// Privacy: page event payloads (including text increments)
// exist only in session memory. Nothing about page content,
// URLs, input text or CDP traffic is logged or persisted; the
// unified log records session lifecycle facts only.
type SyncService struct {
	instances *InstanceService
	Auditor   *logging.System

	mu   sync.Mutex
	sess *syncSession

	// Window arrangement state, usable with or without a
	// session. savedPlacements keeps each arranged window's
	// original placement — together with the identity it was
	// captured from — until it has been restored.
	savedMu         sync.Mutex
	savedPlacements map[uintptr]savedWindow

	// setWindowPlacement is indirect for tests.
	setWindowPlacement func(hwnd uintptr, p winmgr.Placement) error
	// verifySavedWindow re-validates a saved window's identity
	// before restore (indirect for tests).
	verifySavedWindow func(hwnd uintptr, rec savedWindow) bool
}

// savedWindow is one window's pre-arrange placement PLUS the
// identity it was captured from. A bare HWND is never trusted
// at restore time: Windows may recycle handles, so restore
// re-verifies the instance, PID, process creation time and
// the window's current owner before touching anything.
type savedWindow struct {
	InstanceID string
	PID        int
	CreatedAt  int64
	Placement  winmgr.Placement
}

// NewSyncService builds the service on top of the instance
// runtime (the only source of process identity and CDP ports).
func NewSyncService(instances *InstanceService) *SyncService {
	s := &SyncService{
		instances:          instances,
		savedPlacements:    map[uintptr]savedWindow{},
		setWindowPlacement: winmgr.SetPlacement,
	}
	s.verifySavedWindow = s.defaultVerifySavedWindow
	return s
}

// Sync phases (the session state machine):
//
//	stopped → starting → running ⇄ (paused via reasons) → stopping → stopped
//
// Pausing is not a phase: it is a set of reasons (manual, or
// system reasons such as an open dialog) over the running
// phase, so a system pause ending can never clear a manual
// pause.
const (
	syncStopped  = "stopped"
	syncStarting = "starting"
	syncRunning  = "running"
	syncStopping = "stopping"
)

// SyncParticipantView is one instance's state in the UI.
type SyncParticipantView struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	Role       string `json:"role"`   // master / follower / ""
	Status     string `json:"status"` // 同步中 / 已暂停 / 失败 / 未参与
	Reason     string `json:"reason"`
}

// SyncStateView is the whole synchronization state for the UI.
type SyncStateView struct {
	Phase        string                `json:"phase"` // 已停止 / 启动中 / 同步中 / 已暂停 / 停止中
	MasterID     string                `json:"masterId"`
	MasterName   string                `json:"masterName"`
	ManualStop   bool                  `json:"-"`
	Participants []SyncParticipantView `json:"participants"`
	// MirrorActive reports whether master window mirroring is
	// running this session.
	MirrorActive bool `json:"mirrorActive"`
}

// SyncCandidate is one instance's synchronization capability.
type SyncCandidate struct {
	InstanceID   string `json:"instanceId"`
	Name         string `json:"name"`
	KernelName   string `json:"kernelName"`
	Running      bool   `json:"running"`
	Verified     bool   `json:"verified"`
	CDPAvailable bool   `json:"cdpAvailable"`
	Reason       string `json:"reason"`
}

// GetSyncCandidates lists every instance with its sync
// capability, straight from the instance runtime.
func (s *SyncService) GetSyncCandidates(operationID string) ([]SyncCandidate, error) {
	op := beginOp(s.Auditor, operationID, "sync", "candidates", "", "")
	list, err := s.instances.ListInstances()
	if err != nil {
		finishOp(op, err)
		return nil, err
	}
	out := make([]SyncCandidate, 0, len(list))
	for _, inst := range list {
		c := SyncCandidate{InstanceID: inst.ID, Name: inst.Name, KernelName: inst.KernelName}
		info, ok := s.instances.SyncRuntimeInfo(inst.ID)
		switch {
		case !ok:
			c.Reason = "实例不存在"
		case !info.Running:
			c.Reason = "实例未运行"
		case !info.Verified:
			c.Reason = "实例运行状态无法确认"
		case info.CDPPort == 0:
			c.Reason = "实例没有可用的调试端口，请重新启动该实例"
		default:
			c.Running, c.Verified, c.CDPAvailable = true, true, true
		}
		out = append(out, c)
	}
	finishOp(op, nil)
	return out, nil
}

// syncSession is one running synchronization session.
type syncSession struct {
	id       string
	masterID string
	master   SyncRuntimeInfo

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	phase        string
	manualPaused bool
	systemPauses map[string]bool
	tornDown     bool
	failReason   string

	masterClient *cdp.Client
	masterTrack  *pageTracker
	activeTarget string

	followersMu sync.RWMutex
	followers   map[string]*followerWorker
	order       []string

	// failedFollowers remembers start-time and runtime
	// failure reasons by follower ID (nil worker entries in
	// the followers map are start-time failures).
	failedFollowers map[string]string

	// navigation dedup (session memory only)
	navMu   sync.Mutex
	navSeen map[string]time.Time

	// composition twin dedup
	compMu      sync.Mutex
	compPending bool
	compText    string

	// window mirroring
	masterHWND    uintptr
	followerHWNDs map[string]uintptr
	lastMasterPl  *winmgr.Placement
	lastMasterScr *winmgr.Rect
}

func (sess *syncSession) pausedLocked() bool {
	return sess.manualPaused || len(sess.systemPauses) > 0
}

func (sess *syncSession) live() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.phase == syncRunning && !sess.pausedLocked() && !sess.tornDown
}

func (sess *syncSession) phaseLabel() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	switch sess.phase {
	case syncStarting:
		return "启动中"
	case syncRunning:
		if sess.pausedLocked() {
			return "已暂停"
		}
		return "同步中"
	case syncStopping:
		return "停止中"
	}
	return "已停止"
}

func (sess *syncSession) setSystemPause(reason string, on bool) {
	sess.mu.Lock()
	was := sess.pausedLocked()
	if on {
		sess.systemPauses[reason] = true
	} else {
		delete(sess.systemPauses, reason)
	}
	now := sess.pausedLocked()
	sess.mu.Unlock()
	if was == now {
		return
	}
	if now {
		sess.forEachFollower(func(w *followerWorker) {
			w.requestPurgeAndRelease()
		})
	} else {
		// Resuming from a system pause: re-confirm the active
		// target before events flow again.
		sess.refreshActive()
	}
}

// followersSnapshot copies the follower map under its lock;
// callers then work on the copy without holding the lock
// during CDP or window operations.
func (sess *syncSession) followersSnapshot() map[string]*followerWorker {
	sess.followersMu.RLock()
	defer sess.followersMu.RUnlock()
	out := make(map[string]*followerWorker, len(sess.followers))
	for id, w := range sess.followers {
		out[id] = w
	}
	return out
}

// addFollower is the only writer of the followers map.
func (sess *syncSession) addFollower(id string, w *followerWorker) {
	sess.followersMu.Lock()
	defer sess.followersMu.Unlock()
	if _, dup := sess.followers[id]; !dup && w != nil {
		sess.order = append(sess.order, id)
	}
	sess.followers[id] = w
}

func (sess *syncSession) forEachFollower(fn func(*followerWorker)) {
	for _, w := range sess.followersSnapshot() {
		if w != nil {
			fn(w)
		}
	}
}

func (sess *syncSession) followerByID(id string) *followerWorker {
	sess.followersMu.RLock()
	defer sess.followersMu.RUnlock()
	return sess.followers[id]
}

// dispatch fans one master page event out to every healthy
// follower, applying the composition twin dedup first.
func (sess *syncSession) dispatch(ev PageEvent) {
	if !sess.live() {
		return
	}
	// Composition dedup: Chromium delivers the IME result as a
	// compositionend and then AGAIN as an input increment with
	// the same text. The twin is dropped exactly once.
	sess.compMu.Lock()
	if ev.Kind == "input" && sess.compPending && ev.Text == sess.compText {
		sess.compPending = false
		sess.compMu.Unlock()
		return
	}
	if ev.Kind == "compositionend" {
		sess.compPending = true
		sess.compText = ev.Text
	} else if ev.Kind != "input" {
		sess.compPending = false
	}
	sess.compMu.Unlock()

	sess.forEachFollower(func(w *followerWorker) {
		w.enqueue(ev)
	})
}

// refreshActive re-derives the active master target from page
// visibility and, when it changed, points every follower at
// the corresponding target.
func (sess *syncSession) refreshActive() {
	if sess.masterTrack == nil {
		return
	}
	active := sess.masterTrack.VisibleTarget()
	sess.mu.Lock()
	changed := active != "" && active != sess.activeTarget
	if changed {
		sess.activeTarget = active
	}
	sess.mu.Unlock()
	if active == "" {
		sess.setSystemPause("target", true)
		return
	}
	sess.setSystemPause("target", false)
	if !changed {
		return
	}
	url := sess.masterTrack.URL(active)
	sess.forEachFollower(func(w *followerWorker) {
		if _, ok := w.mappedTarget(active); !ok {
			ev := PageEvent{Kind: "targetcreated", Source: active, Text: url}
			w.enqueue(ev)
		}
		w.enqueue(PageEvent{Kind: "targetactivated", Source: active})
	})
	go sess.refreshViewports(active)
}

func (sess *syncSession) refreshViewports(masterTarget string) {
	mw, mh, ok := viewportOf(sess.masterClient, sess.masterTrack, masterTarget)
	if !ok {
		return
	}
	sess.forEachFollower(func(w *followerWorker) {
		if ft, ok := w.mappedTarget(masterTarget); ok {
			if fw, fh, ok := viewportOf(w.client, w.track, ft); ok {
				w.setViewports(mw, mh, fw, fh)
			}
		}
	})
}

// viewportOf reads one page's CSS layout viewport size.
func viewportOf(client *cdp.Client, track *pageTracker, targetID string) (float64, float64, bool) {
	sid, ok := track.Session(targetID)
	if !ok {
		return 0, 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := client.Call(ctx, sid, "Page.getLayoutMetrics", nil)
	if err != nil {
		return 0, 0, false
	}
	var m struct {
		LayoutViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"layoutViewport"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.LayoutViewport.ClientWidth <= 0 {
		return 0, 0, false
	}
	return m.LayoutViewport.ClientWidth, m.LayoutViewport.ClientHeight, true
}

// StartSync validates the selection, connects every
// participant, builds the initial target mappings and starts
// dispatching. A follower that cannot start is reported as
// failed and does not block the others; a master failure
// aborts the whole start with everything cleaned up.
func (s *SyncService) StartSync(operationID string, masterID string, followerIDs []string) (SyncStateView, error) {
	op := beginOp(s.Auditor, operationID, "sync", "start", "instance", masterID)
	view, err := s.start(masterID, followerIDs)
	finishOp(op, err)
	if err != nil {
		return SyncStateView{}, err
	}
	return view, nil
}

func (s *SyncService) start(masterID string, followerIDs []string) (SyncStateView, error) {
	if strings.TrimSpace(masterID) == "" {
		return SyncStateView{}, fmt.Errorf("请选择主实例")
	}
	seen := map[string]bool{}
	followers := make([]string, 0, len(followerIDs))
	for _, id := range followerIDs {
		if id == masterID {
			return SyncStateView{}, fmt.Errorf("主实例不能同时作为跟随实例")
		}
		if !seen[id] {
			seen[id] = true
			followers = append(followers, id)
		}
	}
	if len(followers) == 0 {
		return SyncStateView{}, fmt.Errorf("请至少选择一个跟随实例")
	}

	s.mu.Lock()
	if s.sess != nil {
		s.mu.Unlock()
		return SyncStateView{}, fmt.Errorf("同步会话已经在运行")
	}
	sess := &syncSession{
		id:            fmt.Sprintf("sync-%d", time.Now().UnixNano()),
		masterID:      masterID,
		phase:         syncStarting,
		systemPauses:  map[string]bool{},
		followers:     map[string]*followerWorker{},
		navSeen:       map[string]time.Time{},
		followerHWNDs: map[string]uintptr{},
	}
	s.sess = sess
	s.mu.Unlock()

	fail := func(err error) (SyncStateView, error) {
		s.teardown(sess, err.Error())
		return SyncStateView{}, err
	}

	masterInfo, ok := s.instances.SyncRuntimeInfo(masterID)
	if !ok || !masterInfo.Running || !masterInfo.Verified {
		return fail(fmt.Errorf("主实例未运行或运行状态无法确认"))
	}
	if masterInfo.CDPPort == 0 {
		return fail(fmt.Errorf("主实例没有可用的调试端口，请重新启动该实例后再同步"))
	}
	sess.master = masterInfo

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	masterClient, err := dialInstance(ctx, masterInfo)
	if err != nil {
		return fail(fmt.Errorf("连接主实例调试通道失败：%w", err))
	}
	sess.masterClient = masterClient
	sess.ctx, sess.cancel = context.WithCancel(context.Background())

	// Discover and attach the master's existing page targets.
	if _, err := masterClient.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		return fail(fmt.Errorf("发现主实例标签失败：%w", err))
	}
	sess.masterTrack = newPageTracker(masterClient, true,
		func(targetID string, ev PageEvent) { s.onMasterEvent(sess, targetID, ev) },
		func(targetID string) { sess.refreshActive() })
	targets, err := listPageTargets(ctx, masterClient)
	if err != nil {
		return fail(fmt.Errorf("读取主实例标签失败：%w", err))
	}
	for _, tid := range targets {
		if err := sess.masterTrack.Attach(ctx, tid); err != nil {
			return fail(fmt.Errorf("接入主实例标签失败：%w", err))
		}
	}
	// Give visibility reports a moment to arrive, then require
	// a uniquely confirmed active target.
	deadline := time.Now().Add(2 * time.Second)
	for sess.masterTrack.VisibleTarget() == "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	active := sess.masterTrack.VisibleTarget()
	if active == "" {
		return fail(fmt.Errorf("无法确认主实例当前显示的标签页，已取消开始同步"))
	}
	sess.activeTarget = active
	s.wireMaster(sess)

	// Followers: connect, pair with the active target, start
	// their dispatchers. Failures are per-follower.
	activeURL := sess.masterTrack.URL(active)
	started := 0
	for _, fid := range followers {
		w, reason := s.startFollower(sess, fid, active, activeURL)
		if w == nil {
			sess.addFollower(fid, nil) // remembered as failed below
			sess.noteFollowerFailed(fid, reason)
			continue
		}
		sess.addFollower(fid, w)
		started++
	}
	if started == 0 {
		return fail(fmt.Errorf("没有跟随实例能够开始同步"))
	}
	sess.refreshViewports(active)

	// Identity watchdog + window mirroring.
	go s.watchdog(sess)
	s.startMirror(sess)

	sess.mu.Lock()
	sess.phase = syncRunning
	sess.mu.Unlock()
	return s.state(), nil
}

func (sess *syncSession) noteFollowerFailed(id, reason string) {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.failedFollowers == nil {
		sess.failedFollowers = map[string]string{}
	}
	sess.failedFollowers[id] = reason
}

// startFollower connects one follower and pairs it with the
// master's active target. Returns (nil, reason) on failure.
func (s *SyncService) startFollower(sess *syncSession, followerID, activeMasterTarget, activeURL string) (*followerWorker, string) {
	info, ok := s.instances.SyncRuntimeInfo(followerID)
	if !ok || !info.Running || !info.Verified {
		return nil, "实例未运行或运行状态无法确认"
	}
	if info.CDPPort == 0 {
		return nil, "实例没有可用的调试端口，请重新启动该实例"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client, err := dialInstance(ctx, info)
	if err != nil {
		return nil, "连接调试通道失败"
	}
	w := newFollowerWorker(followerID, client, func(id, reason string) {
		sess.noteFollowerFailed(id, reason)
	})
	if _, err := client.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true}); err != nil {
		client.Close()
		return nil, "发现标签失败"
	}
	ft, err := w.ensureTarget(ctx, activeMasterTarget, activeURL)
	if err != nil {
		client.Close()
		return nil, "创建对应标签失败"
	}
	w.pairWith(activeMasterTarget, ft)
	// A JavaScript dialog on a follower is never answered
	// automatically (the user might cancel on the master
	// while a follower accepted — the operations would
	// diverge). It pauses the session like a master dialog;
	// when it closes, target state is re-confirmed through
	// the normal activation refresh before dispatch resumes.
	client.On("Page.javascriptDialogOpening", func(ev cdp.Event) {
		sess.setSystemPause("dialog-follower", true)
	})
	client.On("Page.javascriptDialogClosed", func(ev cdp.Event) {
		sess.setSystemPause("dialog-follower", false)
	})
	w.start(sess.ctx)
	return w, ""
}

// dialInstance discovers and dials one instance's CDP
// endpoint, both steps loopback-validated.
func dialInstance(ctx context.Context, info SyncRuntimeInfo) (*cdp.Client, error) {
	wsURL, err := cdp.Discover(ctx, info.CDPPort)
	if err != nil {
		return nil, err
	}
	return cdp.Dial(ctx, wsURL)
}

// listPageTargets returns the browser's current page target
// IDs.
func listPageTargets(ctx context.Context, client *cdp.Client) ([]string, error) {
	raw, err := client.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.TargetInfos))
	for _, ti := range out.TargetInfos {
		if ti.Type == "page" {
			ids = append(ids, ti.TargetID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// PauseSync records the manual pause: dispatch stops, pending
// moves/wheels are dropped and pressed inputs are released.
// Nothing is replayed later.
func (s *SyncService) PauseSync(operationID string) (SyncStateView, error) {
	op := beginOp(s.Auditor, operationID, "sync", "pause", "", "")
	sess := s.current()
	if sess == nil {
		err := fmt.Errorf("同步会话未在运行")
		finishOp(op, err)
		return SyncStateView{}, err
	}
	sess.mu.Lock()
	sess.manualPaused = true
	sess.mu.Unlock()
	sess.forEachFollower(func(w *followerWorker) {
		w.requestPurgeAndRelease()
	})
	finishOp(op, nil)
	return s.state(), nil
}

// ResumeSync clears the manual pause after re-verifying every
// participant's identity and the target mappings.
func (s *SyncService) ResumeSync(operationID string) (SyncStateView, error) {
	op := beginOp(s.Auditor, operationID, "sync", "resume", "", "")
	sess := s.current()
	if sess == nil {
		err := fmt.Errorf("同步会话未在运行")
		finishOp(op, err)
		return SyncStateView{}, err
	}
	if !s.instances.VerifySyncIdentity(sess.master) {
		err := fmt.Errorf("主实例已退出或身份无法确认，同步会话结束")
		s.teardown(sess, err.Error())
		finishOp(op, err)
		return SyncStateView{}, err
	}
	sess.forEachFollower(func(w *followerWorker) {
		if w == nil {
			return
		}
		if info, ok := s.instances.SyncRuntimeInfo(w.id); !ok || !s.instances.VerifySyncIdentity(info) {
			w.markFailed("实例已退出或身份无法确认")
		}
	})
	sess.mu.Lock()
	sess.manualPaused = false
	sess.mu.Unlock()
	sess.forEachFollower(func(w *followerWorker) {
		if w != nil {
			w.clearPending()
		}
	})
	sess.refreshActive()
	finishOp(op, nil)
	return s.state(), nil
}

// StopSync ends the session and releases every resource.
func (s *SyncService) StopSync(operationID string) (SyncStateView, error) {
	op := beginOp(s.Auditor, operationID, "sync", "stop", "", "")
	sess := s.current()
	if sess == nil {
		finishOp(op, nil)
		return s.state(), nil
	}
	s.teardown(sess, "")
	finishOp(op, nil)
	return s.state(), nil
}

func (s *SyncService) current() *syncSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess
}

// teardown releases a session completely: hook, connections,
// workers, mappings and mirroring. Browsers keep running.
func (s *SyncService) teardown(sess *syncSession, reason string) {
	s.mu.Lock()
	if s.sess == sess {
		s.sess = nil
	}
	s.mu.Unlock()

	sess.mu.Lock()
	if sess.tornDown {
		sess.mu.Unlock()
		return
	}
	sess.tornDown = true
	sess.phase = syncStopping
	sess.failReason = reason
	sess.mu.Unlock()

	// Each worker's close first releases its pressed inputs
	// through its own serial dispatcher, then stops it.
	sess.forEachFollower(func(w *followerWorker) {
		w.close()
	})
	if sess.masterClient != nil {
		sess.masterClient.Close()
	}
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.mu.Lock()
	sess.phase = syncStopped
	sess.mu.Unlock()
}

// GetSyncState returns the current state for the UI.
func (s *SyncService) GetSyncState(operationID string) (SyncStateView, error) {
	return s.state(), nil
}

func (s *SyncService) state() SyncStateView {
	sess := s.current()
	names := map[string]string{}
	if list, err := s.instances.ListInstances(); err == nil {
		for _, inst := range list {
			names[inst.ID] = inst.Name
		}
	}
	if sess == nil {
		return SyncStateView{Phase: "已停止", Participants: []SyncParticipantView{}}
	}
	view := SyncStateView{
		Phase:        sess.phaseLabel(),
		MasterID:     sess.masterID,
		MasterName:   names[sess.masterID],
		MirrorActive: sess.masterHWND != 0,
		Participants: []SyncParticipantView{},
	}
	view.Participants = append(view.Participants, SyncParticipantView{
		InstanceID: sess.masterID, Name: names[sess.masterID], Role: "master", Status: "同步中",
	})
	snapshot := sess.followersSnapshot()
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := SyncParticipantView{InstanceID: id, Name: names[id], Role: "follower", Status: "同步中"}
		if w := snapshot[id]; w == nil || w.isFailed() {
			p.Status = "失败"
			p.Reason = sess.failedReason(id)
		} else if sess.phaseLabel() == "已暂停" {
			p.Status = "已暂停"
		}
		view.Participants = append(view.Participants, p)
	}
	return view
}

func (sess *syncSession) failedReason(id string) string {
	sess.mu.Lock()
	if sess.failedFollowers != nil {
		if r := sess.failedFollowers[id]; r != "" {
			sess.mu.Unlock()
			return r
		}
	}
	sess.mu.Unlock()
	if w := sess.followerByID(id); w != nil {
		return w.failureReason()
	}
	return "同步失败"
}
