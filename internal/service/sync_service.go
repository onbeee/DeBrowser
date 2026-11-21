package service

import (
	"context"
	"encoding/json"
	"errors"
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

	// winMu serializes every window-geometry mutation:
	// arranging, restoring and the session's window mirror
	// must never move the same windows at the same time.
	winMu sync.Mutex

	// setWindowPlacement is indirect for tests.
	setWindowPlacement func(hwnd uintptr, p winmgr.Placement) error
	// verifySavedWindow re-validates a saved window's identity
	// before restore (indirect for tests).
	verifySavedWindow func(hwnd uintptr, rec savedWindow) bool
	// getWindowGeometry reads a window's placement and screen
	// rectangle together (indirect for tests).
	getWindowGeometry func(hwnd uintptr) (winmgr.Placement, winmgr.Rect, error)
	// setWindowRect applies a screen-coordinate rectangle
	// (indirect for tests).
	setWindowRect func(hwnd uintptr, r winmgr.Rect) error
	// windowOwnerPID reads a window's owner PID (indirect
	// for tests).
	windowOwnerPID func(hwnd uintptr) int
}

// defaultWindowGeometry reads placement and screen rectangle.
func defaultWindowGeometry(hwnd uintptr) (winmgr.Placement, winmgr.Rect, error) {
	pl, err := winmgr.GetPlacement(hwnd)
	if err != nil {
		return winmgr.Placement{}, winmgr.Rect{}, err
	}
	scr, err := winmgr.WindowScreenRect(hwnd)
	if err != nil {
		return winmgr.Placement{}, winmgr.Rect{}, err
	}
	return pl, scr, nil
}

func (s *SyncService) geometryOf(hwnd uintptr) (winmgr.Placement, winmgr.Rect, error) {
	if s.getWindowGeometry != nil {
		return s.getWindowGeometry(hwnd)
	}
	return defaultWindowGeometry(hwnd)
}

func (s *SyncService) applyWindowRect(hwnd uintptr, r winmgr.Rect) error {
	if s.setWindowRect != nil {
		return s.setWindowRect(hwnd, r)
	}
	return winmgr.SetWindowRect(hwnd, r)
}

func (s *SyncService) ownerPID(hwnd uintptr) int {
	if s.windowOwnerPID != nil {
		return s.windowOwnerPID(hwnd)
	}
	return winmgr.WindowOwnerPID(hwnd)
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

	// Master target URLs as last reported by
	// Target.targetCreated / Target.targetInfoChanged
	// (session memory only, never logged). A target whose
	// URL is still empty is NOT yet offered to followers:
	// the creation-instant URL of a new tab is transient,
	// and materializing it produced permanent about:blank
	// tabs on followers (real-machine finding).
	targetMu   sync.Mutex
	targetURLs map[string]string

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
// noteTargetURL records a master target's latest known URL
// from its lifecycle events.
func (sess *syncSession) noteTargetURL(tid, url string) {
	sess.targetMu.Lock()
	defer sess.targetMu.Unlock()
	if sess.targetURLs == nil {
		sess.targetURLs = map[string]string{}
	}
	sess.targetURLs[tid] = url
}

// forgetTargetURL drops a destroyed target's record.
func (sess *syncSession) forgetTargetURL(tid string) {
	sess.targetMu.Lock()
	defer sess.targetMu.Unlock()
	delete(sess.targetURLs, tid)
}

// targetURL returns the last known URL ("" = not known yet).
func (sess *syncSession) targetURL(tid string) string {
	sess.targetMu.Lock()
	defer sess.targetMu.Unlock()
	return sess.targetURLs[tid]
}

// offerTarget asks every follower to create its counterpart
// of a master target — but ONLY once the target's URL is
// known. While the URL is still empty (the transient state
// of a just-created tab), nothing is created; the later
// Target.targetInfoChanged update offers it then. Followers
// that already have the mapping ignore the offer (their
// ensure path is single-flight and mapping-first), so
// repeated offers never duplicate a tab.
func (sess *syncSession) offerTarget(tid string) {
	url := sess.targetURL(tid)
	if url == "" {
		return
	}
	sess.forEachFollower(func(w *followerWorker) {
		w.enqueue(PageEvent{Kind: "targetcreated", Source: tid, Text: url})
	})
}

func (sess *syncSession) refreshActive() {
	if sess.masterTrack == nil {
		return
	}
	active, visibleCount := sess.masterTrack.visibleState()
	if visibleCount != 1 {
		// No unique visible target: pause. The previously
		// confirmed active target is KEPT — a transient
		// invisibility (the master window covered or
		// minimized) must not discard it; a destroyed target
		// is cleared by the destruction handler before this
		// runs, so a stale target is never revived here.
		sess.setSystemPause("target", true)
		return
	}
	sess.setSystemPause("target", false)
	sess.mu.Lock()
	changed := active != sess.activeTarget
	if changed {
		sess.activeTarget = active
	}
	sess.mu.Unlock()
	if !changed {
		return
	}
	sess.offerTarget(active)
	sess.forEachFollower(func(w *followerWorker) {
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
		targetURLs:    map[string]string{},
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
	masterTargets, err := listPageTargets(ctx, masterClient)
	if err != nil {
		return fail(fmt.Errorf("读取主实例标签失败：%w", err))
	}
	for _, ti := range masterTargets {
		// The getTargets URL of an already-open tab is its
		// current, final URL: record it for every target,
		// not just the active one, so start-of-session
		// mapping and later offers use real addresses.
		sess.noteTargetURL(ti.ID, ti.URL)
		if err := sess.masterTrack.Attach(ctx, ti.ID); err != nil {
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
		return fail(fmt.Errorf("无法确认主实例当前显示的标签页。请先确保主实例窗口可见且只有一个活动标签页，再重试。"))
	}
	sess.activeTarget = active
	s.wireMaster(sess)

	// Followers: connect, build the mapping for ALL of the
	// master's pre-existing targets (not only the active
	// one), start their dispatchers. Failures are
	// per-follower.
	started := 0
	for _, fid := range followers {
		w, reason := s.startFollower(sess, fid, masterTargets, active)
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
	// A tab created on the master while followers were still
	// starting had no follower to be offered to yet: re-list
	// and offer every known target once. Offers are
	// mapping-first and single-flight, so targets that were
	// mapped during start are not duplicated.
	s.reconcileMasterTargets(sess)
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

// reconcileMasterTargets re-reads the master's page target
// list once after all followers have started and offers
// every target whose URL is known. It closes the gap for a
// tab created while followers were still starting (its
// targetCreated event fired before any follower existed to
// receive the offer). Already-mapped targets are recognized
// by the workers' mapping-first ensure path, so nothing is
// created twice; a target whose URL is still unknown is
// left to the targetInfoChanged path, as before.
func (s *SyncService) reconcileMasterTargets(sess *syncSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	targets, err := listPageTargets(ctx, sess.masterClient)
	if err != nil {
		return
	}
	for _, ti := range targets {
		if ti.URL != "" {
			sess.noteTargetURL(ti.ID, ti.URL)
		}
		_ = sess.masterTrack.Attach(ctx, ti.ID)
		sess.offerTarget(ti.ID)
	}
}

// startFollower connects one follower and builds its initial
// mapping for ALL of the master's pre-existing page targets.
// Returns (nil, reason) on failure.
func (s *SyncService) startFollower(sess *syncSession, followerID string, masterTargets []pageTargetInfo, activeMasterTarget string) (*followerWorker, string) {
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
	// Initial mapping for every pre-existing master tab:
	// the follower's own existing tabs are reused first and
	// new tabs are created only where no counterpart can be
	// reused. A tab whose correspondence cannot be
	// established reliably fails THIS follower's start with
	// a clear reason — mappings are never guessed.
	if err := w.initTargets(ctx, masterTargets, activeMasterTarget); err != nil {
		client.Close()
		if errors.Is(err, errInitialMapping) {
			return nil, errInitialMapping.Error() + "，请关闭多余标签页后重试"
		}
		return nil, "建立已有标签页对应关系失败"
	}
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

// pageTargetInfo is one browser page target as reported by
// Target.getTargets: its ID and its CURRENT URL. The URL of
// an already-open tab is final here (unlike the transient
// creation-instant URL of Target.targetCreated), which is
// what makes start-of-session mapping possible.
type pageTargetInfo struct {
	ID  string
	URL string
}

// listPageTargets returns the browser's current page targets
// in the order the browser reports them. Non-page targets
// (extensions, DevTools, background pages) are excluded. The
// reported order is preserved — initial mapping pairs
// same-URL tabs by their ordinal position in this order, so
// it must never be re-sorted or shuffled.
func listPageTargets(ctx context.Context, client *cdp.Client) ([]pageTargetInfo, error) {
	raw, err := client.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	infos := make([]pageTargetInfo, 0, len(out.TargetInfos))
	for _, ti := range out.TargetInfos {
		if ti.Type == "page" {
			infos = append(infos, pageTargetInfo{ID: ti.TargetID, URL: ti.URL})
		}
	}
	return infos, nil
}

// errInitialMapping marks a start-of-session tab set whose
// one-to-one correspondence cannot be established reliably.
var errInitialMapping = fmt.Errorf("无法确认主实例已有标签页与跟随实例标签页的对应关系")

// planInitialMapping deterministically pairs the master's
// pre-existing page targets with the follower's, one to one:
//
//   - A master tab pairs with the first still-unpaired
//     follower tab showing the SAME URL, both sides taken in
//     reported order — so several tabs on the same URL pair
//     by ordinal position and never share one follower tab.
//   - If exactly one master tab and one follower tab remain
//     unpaired afterwards, they pair by elimination (the
//     totals then necessarily match): this covers a tab
//     whose URL is not yet known on one side, e.g. a fresh
//     new-tab page.
//   - A master tab that remains unpaired and has a known URL
//     is returned in toCreate: the follower gets a new tab.
//   - A master tab that remains unpaired with NO known URL
//     can neither be paired nor created without guessing —
//     the whole pairing fails with errInitialMapping and the
//     follower does not start, rather than silently building
//     a wrong mapping.
//
// Follower tabs left unpaired are the follower's own
// pre-existing tabs: they stay untouched and unmapped.
func planInitialMapping(master, follower []pageTargetInfo) (pairs map[string]string, toCreate []pageTargetInfo, err error) {
	pairs = map[string]string{}
	used := map[string]bool{}
	byURL := map[string][]string{}
	for _, f := range follower {
		if f.URL != "" {
			byURL[f.URL] = append(byURL[f.URL], f.ID)
		}
	}
	var restM []pageTargetInfo
	for _, m := range master {
		if m.URL != "" {
			if q := byURL[m.URL]; len(q) > 0 {
				pairs[m.ID] = q[0]
				used[q[0]] = true
				byURL[m.URL] = q[1:]
				continue
			}
		}
		restM = append(restM, m)
	}
	var restF []pageTargetInfo
	for _, f := range follower {
		if !used[f.ID] {
			restF = append(restF, f)
		}
	}
	if len(restM) == 1 && len(restF) == 1 {
		m, f := restM[0], restF[0]
		if m.URL == "" || f.URL == "" || m.URL == f.URL {
			pairs[m.ID] = f.ID
			restM, restF = nil, nil
		}
	}
	for _, m := range restM {
		if m.URL == "" {
			return nil, nil, errInitialMapping
		}
		toCreate = append(toCreate, m)
	}
	return pairs, toCreate, nil
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
	// Resume succeeds only when the session is REALLY back
	// in the running, unpaused phase. A surviving system
	// pause (unconfirmed active target, an open dialog) is
	// reported with its reason — never cleared by force and
	// never reported as a successful resume.
	if reason := sess.pauseReasonText(); reason != "" {
		err := fmt.Errorf("同步仍处于暂停状态：%s", reason)
		finishOp(op, err)
		return s.state(), err
	}
	finishOp(op, nil)
	return s.state(), nil
}

// pauseReasonText describes why the session is not in the
// plain running state, or "" when it is running unpaused.
// System pause keys are internal; the user-facing text is
// fixed per known key.
func (sess *syncSession) pauseReasonText() string {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.phase != syncRunning {
		return "同步会话尚未进入运行状态"
	}
	var parts []string
	if sess.manualPaused {
		parts = append(parts, "已手动暂停")
	}
	keys := make([]string, 0, len(sess.systemPauses))
	for k := range sess.systemPauses {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "target":
			parts = append(parts, "无法确认主实例当前显示的标签页")
		case "dialog-master":
			parts = append(parts, "主实例有未处理的对话框")
		case "dialog-follower":
			parts = append(parts, "跟随实例有未处理的对话框")
		default:
			parts = append(parts, "系统原因")
		}
	}
	return strings.Join(parts, "；")
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

	// Wait out a window-mirror tick that already passed its
	// live check: mirrorOnce holds winMu for its whole apply
	// phase, so acquiring it here guarantees no mirror is
	// still moving follower windows while the session is
	// torn down — StopSync must not return with one last
	// mirror application still in flight. A tick arriving
	// after the tornDown mark fails its live check and
	// returns immediately, so this never waits on future
	// ticks. Lock order: no caller of teardown holds winMu,
	// and mirrorOnce never calls teardown.
	s.winMu.Lock()
	s.winMu.Unlock()

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

// teardownMasterGone ends a session whose master went away
// (its CDP connection dropped, or the watchdog found its
// identity gone). When the instance runtime CONFIRMS the
// master process is no longer running, every master tab is
// gone with it — including the last one, whose destruction
// event may never have arrived because the connection died
// with the process. A closure is therefore queued for every
// mapped master target before teardown: closures that the
// real destruction event already queued are recognized as
// duplicates by the workers (mapping-first, dropped after
// closing), and each worker's close drains its queue so the
// closures are executed before its connection closes — the
// followers end with no mapped tab left behind. When the
// master process is NOT confirmed gone (e.g. only the debug
// connection dropped), no tab is touched: the session simply
// ends, exactly like a manual stop.
func (s *SyncService) teardownMasterGone(sess *syncSession, reason string) {
	gone := true
	if s.instances != nil {
		if info, ok := s.instances.SyncRuntimeInfo(sess.masterID); ok && info.Running {
			gone = false
		}
	}
	if gone {
		sess.forEachFollower(func(w *followerWorker) {
			for _, mt := range w.mappedMasterTargets() {
				w.enqueue(PageEvent{Kind: "targetclosed", Source: mt})
			}
		})
	}
	s.teardown(sess, reason)
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
	// Every participant's status derives from the ONE real
	// session phase: the master never reports 同步中 while
	// the session is starting, paused or stopping. A
	// follower's own failure stays its independent status
	// and is never overwritten by the session phase.
	label := sess.phaseLabel()
	view := SyncStateView{
		Phase:        label,
		MasterID:     sess.masterID,
		MasterName:   names[sess.masterID],
		MirrorActive: sess.masterHWND != 0,
		Participants: []SyncParticipantView{},
	}
	view.Participants = append(view.Participants, SyncParticipantView{
		InstanceID: sess.masterID, Name: names[sess.masterID], Role: "master", Status: label,
	})
	snapshot := sess.followersSnapshot()
	ids := make([]string, 0, len(snapshot))
	for id := range snapshot {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := SyncParticipantView{InstanceID: id, Name: names[id], Role: "follower", Status: label}
		if w := snapshot[id]; w == nil || w.isFailed() {
			p.Status = "失败"
			p.Reason = sess.failedReason(id)
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
