package service

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// Stage 10 v3 multi-instance synchronization: a CDP master–
// follower design. The master browser's page input is observed by
// a script injected into an isolated world of each page and
// reported through a CDP Runtime binding; followers execute the
// same input on their own CDP connections. Target identity is
// established only from CDP session/target IDs and explicit
// creation results — never from tab titles, URLs or list order.
// The active tab of each instance is the page target that reports
// itself visible through the Page Visibility API.
//
// There is no global input hook, no SendInput, no UI Automation
// and no window activation anywhere in this design: browser-chrome
// UI is handled only through CDP target management, and anything
// CDP cannot express is documented as unsupported rather than
// simulated.

// Sync phases reported to the frontend.
const (
	syncPhaseIdle    = "idle"
	syncPhaseRunning = "running"
	syncPhasePaused  = "paused"
)

// SyncFollowerState is one follower's reported state.
type SyncFollowerState struct {
	InstanceID string `json:"instanceId"`
	Connected  bool   `json:"connected"`
	PageReady  bool   `json:"pageReady"`
	State      string `json:"state"` // ready | unconfirmed | failed
	Reason     string `json:"reason"`
}

// SyncState is the sync page's view of the session.
type SyncState struct {
	Phase           string              `json:"phase"`
	MasterID        string              `json:"masterId"`
	MasterConnected bool                `json:"masterConnected"`
	MasterPageReady bool                `json:"masterPageReady"`
	Followers       []SyncFollowerState `json:"followers"`
}

// SyncMonitor is one display's work area for the arrange feature.
type SyncMonitor struct {
	Index   int     `json:"index"`
	Width   int32   `json:"width"`
	Height  int32   `json:"height"`
	Scale   float64 `json:"scale"`
	Primary bool    `json:"primary"`
}

// SyncService is the Stage 10 binding service.
type SyncService struct {
	instances *InstanceService
	log       *slog.Logger

	// Auditor is wired by main after construction (see the other
	// services): session lifecycle facts go to the unified
	// recording system; input events and text never do.
	Auditor *logging.System

	mu              sync.Mutex
	session         *syncSession
	savedPlacements map[string]savedPlacement
}

type savedPlacement struct {
	hwnd      uintptr
	placement winmgr.Placement
}

// NewSyncService creates the sync service over the instance
// service (the source of running state and debugging ports).
func NewSyncService(instances *InstanceService, log *slog.Logger) *SyncService {
	if log == nil {
		log = slog.Default()
	}
	return &SyncService{
		instances:       instances,
		log:             log,
		savedPlacements: make(map[string]savedPlacement),
	}
}

// trackerMux routes connection events to the tracker created for
// that connection (the tracker exists only after Dial returns).
type trackerMux struct{ tr atomic.Pointer[pageTracker] }

func (m *trackerMux) handle(ev cdp.Event) {
	if t := m.tr.Load(); t != nil {
		t.HandleEvent(ev)
	}
}

func dialBrowser(ctx context.Context, port int, cb pageCallbacks) (*cdp.Client, *pageTracker, error) {
	wsURL, err := cdp.Discover(ctx, port)
	if err != nil {
		return nil, nil, err
	}
	mux := &trackerMux{}
	client, err := cdp.Dial(ctx, wsURL, mux.handle)
	if err != nil {
		return nil, nil, err
	}
	tracker := newPageTracker(client, cb)
	mux.tr.Store(tracker)
	if err := tracker.Start(ctx); err != nil {
		_ = client.Close()
		return nil, nil, err
	}
	return client, tracker, nil
}

// waitActiveTarget polls a tracker until exactly one page target
// reports visible, or the timeout passes.
func waitActiveTarget(tracker *pageTracker, timeout time.Duration) (string, PageView, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if id, view, ok := tracker.ActiveTarget(); ok {
			return id, view, true
		}
		if time.Now().After(deadline) {
			return "", PageView{}, false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// syncSession is one running or paused sync session.
type syncSession struct {
	service *SyncService

	phase    string
	masterID string

	masterClient  *cdp.Client
	masterTracker *pageTracker

	mu           sync.Mutex
	masterActive string
	activeKnown  bool
	masterW      float64
	masterH      float64
	// Composition-twin dedup: after a composition text event,
	// an immediately following input text event with identical
	// text is the same commit reported twice and is dropped.
	compTwinPending bool
	compTwinText    string

	switchMu  sync.Mutex
	followers map[string]*followerWorker
	order     []string
	tornDown  atomic.Bool

	// Follower page creation single-flight: for one master
	// target, each follower creates AT MOST ONE counterpart page
	// per session, no matter which path asks first — the
	// TargetCreated callback, a visibility-driven refresh, or a
	// resume re-point. creating holds the in-flight creations;
	// an existing mapping, an in-flight creation and a completed
	// creation are three distinct states, and a failed creation
	// removes its entry so a later trigger can retry cleanly.
	createMu sync.Mutex
	creating map[targetCreationKey]*targetCreation
	// live is set once session setup (master + every follower's
	// initial state) has completed. Follower visibility callbacks
	// may fire during setup; they must not run recovery logic
	// against half-initialised workers, so refreshFollowerActive
	// waits for live.
	live atomic.Bool

	// Window mirroring (Stage 10 window behaviour): the master
	// window's placement is polled and applied to follower
	// windows without ever activating them. Handles are resolved
	// best-effort at session start; a window that cannot be
	// found simply does not take part in mirroring.
	masterHWND    uintptr
	followerHWNDs map[string]uintptr
	mirrorStop    chan struct{}
	lastMasterPl  *winmgr.Placement
	// lastMasterScreen is the master window's last mirrored
	// ACTUAL screen rectangle (from GetWindowRect). Placement
	// rectangles are workspace coordinates and are never used
	// as screen geometry; both are tracked because a move can
	// change one view before the other settles.
	lastMasterScreen *winmgr.Rect
}

// followerByID reads the session's follower map under its lock:
// master event handlers run on tracker goroutines while the map
// is still being populated during session start, so the map is
// never accessed bare.
func (sess *syncSession) followerByID(id string) *followerWorker {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.followers[id]
}

func (sess *syncSession) addFollower(id string, w *followerWorker) {
	sess.mu.Lock()
	sess.followers[id] = w
	sess.mu.Unlock()
}

// tracker reads the master tracker under the session lock: the
// field is assigned after dialBrowser returns, while tracker
// event goroutines may already be running.
func (sess *syncSession) tracker() *pageTracker {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.masterTracker
}

func (sess *syncSession) setTracker(t *pageTracker) {
	sess.mu.Lock()
	sess.masterTracker = t
	sess.mu.Unlock()
}

// StartSync begins a sync session: the master instance's page
// input will be mirrored to the selected followers.
func (s *SyncService) StartSync(operationID string, masterID string, followerIDs []string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "开始同步", "instance", masterID)
	err := s.startSync(masterID, followerIDs)
	finishOp(op, err)
	return err
}

func (s *SyncService) startSync(masterID string, followerIDs []string) error {
	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		return fmt.Errorf("同步会话已在进行中")
	}
	s.mu.Unlock()

	if masterID == "" {
		return fmt.Errorf("请选择主实例")
	}
	if len(followerIDs) == 0 {
		return fmt.Errorf("请至少选择一个跟随实例")
	}
	seen := map[string]bool{masterID: true}
	for _, id := range followerIDs {
		if seen[id] {
			return fmt.Errorf("主实例与跟随实例不能重复")
		}
		seen[id] = true
	}

	list, err := s.instances.ListInstances()
	if err != nil {
		return err
	}
	byID := make(map[string]model.Instance, len(list))
	for _, inst := range list {
		byID[inst.ID] = inst
	}
	checkInstance := func(id string) (int, error) {
		inst, ok := byID[id]
		if !ok {
			return 0, fmt.Errorf("实例不存在")
		}
		if inst.Status != model.StatusRunning {
			return 0, fmt.Errorf("实例 %s 未在运行", inst.Name)
		}
		port, ok := s.instances.instanceCDPPort(id)
		if !ok {
			return 0, fmt.Errorf("实例 %s 没有可用的调试端口（可能由应用重启前的进程接管），请重启该实例后再同步", inst.Name)
		}
		return port, nil
	}
	masterPort, err := checkInstance(masterID)
	if err != nil {
		return err
	}
	followerPorts := make(map[string]int, len(followerIDs))
	for _, id := range followerIDs {
		port, err := checkInstance(id)
		if err != nil {
			return err
		}
		followerPorts[id] = port
	}

	sess := &syncSession{
		service:       s,
		phase:         syncPhaseRunning,
		masterID:      masterID,
		followers:     make(map[string]*followerWorker),
		order:         append([]string(nil), followerIDs...),
		followerHWNDs: make(map[string]uintptr),
		mirrorStop:    make(chan struct{}),
		creating:      make(map[targetCreationKey]*targetCreation),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Master first: the session cannot start unless the master's
	// connection and its active page are both confirmed.
	masterClient, masterTracker, err := dialBrowser(ctx, masterPort, pageCallbacks{
		OnPageEvent:       sess.onMasterPageEvent,
		OnVisibility:      sess.onMasterVisibility,
		OnNavigate:        sess.onMasterNavigate,
		OnTargetCreated:   sess.onMasterTargetCreated,
		OnTargetDestroyed: sess.onMasterTargetDestroyed,
	})
	if err != nil {
		return fmt.Errorf("连接主实例失败：%w", err)
	}
	sess.masterClient = masterClient
	sess.setTracker(masterTracker)
	masterActive, _, ok := waitActiveTarget(masterTracker, 3*time.Second)
	if !ok {
		_ = masterClient.Close()
		return fmt.Errorf("无法确认主实例的活动页面（没有页面报告为当前标签，或存在多个可见窗口），同步未启动")
	}
	sess.mu.Lock()
	sess.masterActive = masterActive
	sess.activeKnown = true
	if w, h, verr := masterTracker.Viewport(ctx, masterActive); verr == nil {
		sess.masterW, sess.masterH = w, h
	}
	masterW, masterH := sess.masterW, sess.masterH
	sess.mu.Unlock()

	// Followers join independently: one follower's failure is
	// recorded on that follower and never blocks the others.
	for _, id := range followerIDs {
		worker := newFollowerWorker(id)
		worker.masterW, worker.masterH = masterW, masterH
		sess.addFollower(id, worker)
		client, tracker, derr := dialBrowser(ctx, followerPorts[id], sess.followerCallbacks(id))
		if derr != nil {
			worker.markFailed("连接跟随实例失败：" + derr.Error())
			go worker.run()
			continue
		}
		worker.client = client
		worker.tracker = tracker
		active, _, found := waitActiveTarget(tracker, 3*time.Second)
		if !found {
			worker.setUnconfirmed("无法确认跟随实例的活动页面")
			go worker.run()
			continue
		}
		if w, h, verr := tracker.Viewport(ctx, active); verr == nil {
			worker.viewportW, worker.viewportH = w, h
		} else {
			worker.setUnconfirmed("无法读取跟随实例的页面尺寸")
			go worker.run()
			continue
		}
		worker.mapTarget(masterActive, active)
		worker.setActiveTarget(active)
		worker.setReady()
		go worker.run()
	}

	// Window mirroring handles, resolved best-effort: a window
	// that cannot be found does not block the page sync session.
	if hwnd, werr := s.windowForInstance(masterID); werr == nil {
		sess.masterHWND = hwnd
	}
	for _, id := range followerIDs {
		if hwnd, werr := s.windowForInstance(id); werr == nil {
			sess.followerHWNDs[id] = hwnd
		}
	}

	s.mu.Lock()
	if s.session != nil { // a concurrent start won the race
		s.mu.Unlock()
		sess.teardown()
		return fmt.Errorf("同步会话已在进行中")
	}
	s.session = sess
	s.mu.Unlock()
	sess.live.Store(true)

	go sess.runWindowMirror()
	go func() {
		<-masterClient.Done()
		sess.service.onMasterDisconnected(sess)
	}()

	s.recordSessionEvent("开始同步", logging.StatusSuccess, masterID)
	return nil
}

// --- master event handlers (called from tracker callbacks; they
// never block the CDP read path) ---

// paused reports whether the session is paused. While paused,
// NO sync dispatch of any kind happens — page input, navigation,
// tab creation/closure/switching and window mirroring all stop,
// and nothing observed during the pause is replayed on resume.
func (sess *syncSession) paused() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.phase == syncPhasePaused
}

func (sess *syncSession) onMasterPageEvent(targetID string, ev PageEvent) {
	sess.mu.Lock()
	if sess.phase != syncPhaseRunning || !sess.activeKnown || targetID != sess.masterActive {
		sess.mu.Unlock()
		return
	}
	// Composition-twin dedup: an IME composition commits its
	// final text once (source "composition"); some engines then
	// fire an input event carrying the same text as the very
	// next event. That twin is dropped — one commit is sent
	// exactly once. Any other event in between clears the
	// marker, so genuine repeated input is never eaten.
	if ev.Kind == "text" && sess.compTwinPending && ev.Source != "composition" && ev.Text == sess.compTwinText {
		sess.compTwinPending = false
		sess.mu.Unlock()
		return
	}
	if ev.Kind == "text" && ev.Source == "composition" {
		sess.compTwinPending = true
		sess.compTwinText = ev.Text
	} else {
		sess.compTwinPending = false
	}
	sess.mu.Unlock()
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		if worker == nil {
			continue
		}
		if ft, ok := worker.mappedTarget(targetID); ok {
			worker.enqueueInput(ev, ft)
		}
	}
}

func (sess *syncSession) onMasterVisibility(string) {
	go sess.refreshMasterActive()
}

// refreshMasterActive re-evaluates which master page is active
// after a visibility change and re-points every follower: mapped
// targets are activated; a pre-existing but unmapped master tab
// gets a NEW follower page created from its current URL (the
// documented no-guessing behaviour), which may leave the follower
// with an extra tab.
func (sess *syncSession) refreshMasterActive() {
	sess.switchMu.Lock()
	defer sess.switchMu.Unlock()
	if sess.tornDown.Load() {
		return
	}
	tracker := sess.tracker()
	if tracker == nil {
		return
	}
	targetID, view, ok := tracker.ActiveTarget()
	if !ok {
		sess.mu.Lock()
		sess.activeKnown = false
		sess.masterActive = ""
		sess.mu.Unlock()
		return
	}
	sess.mu.Lock()
	changed := targetID != sess.masterActive
	sess.masterActive = targetID
	sess.activeKnown = true
	paused := sess.phase == syncPhasePaused
	sess.mu.Unlock()
	if !changed || paused {
		// While paused the bookkeeping above still tracks the
		// master's current page, but NO tab switch is dispatched
		// to any follower; resume re-confirms from this state.
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if w, h, err := tracker.Viewport(ctx, targetID); err == nil {
		sess.mu.Lock()
		sess.masterW, sess.masterH = w, h
		sess.mu.Unlock()
	}
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		if worker == nil {
			continue
		}
		sess.pointFollower(worker, targetID, view.URL)
	}
}

// pointFollower points one follower at the master's active
// target: an existing mapping is activated; an unmapped target
// gets a NEW follower page created from the master's current URL
// (the documented no-guessing behaviour), and the mapping is
// established only after the new page is fully attached and
// ready — never from the createTarget result alone.
// pointFollower points one follower at the master's active
// target: an existing mapping is activated; an unmapped target
// gets a NEW follower page created from the master's current URL
// (the documented no-guessing behaviour), through the shared
// single-flight creation in ensureFollowerTarget, and the
// mapping is established only after the new page is fully
// attached and ready — never from the createTarget result alone.
func (sess *syncSession) pointFollower(worker *followerWorker, targetID, url string) {
	sess.mu.Lock()
	masterW, masterH := sess.masterW, sess.masterH
	sess.mu.Unlock()
	worker.mu.Lock()
	worker.masterW, worker.masterH = masterW, masterH
	worker.mu.Unlock()
	if ft, mapped := worker.mappedTarget(targetID); mapped {
		worker.setActiveTarget(ft)
		worker.enqueueControl(syncAction{kind: actionActivate, target: ft})
		return
	}
	ft, err := sess.ensureFollowerTarget(worker, targetID, url)
	if err != nil {
		return // the creation path already recorded the outcome on the follower
	}
	worker.setActiveTarget(ft)
	worker.enqueueControl(syncAction{kind: actionActivate, target: ft})
}

// targetCreationKey identifies one in-flight follower page
// creation: one master target on one follower instance.
type targetCreationKey struct {
	followerID   string
	masterTarget string
}

// targetCreation is one in-flight (or just-completed) follower
// page creation shared by every caller asking for the same
// master target on the same follower. done is closed exactly
// once by the creator; followerTarget and err are written before
// done closes, so waiters read the shared result safely.
type targetCreation struct {
	done           chan struct{}
	followerTarget string
	err            error
}

// ensureFollowerTarget returns the follower page mapped to one
// master target, creating it if necessary. It is the SINGLE
// creation path for follower pages: the TargetCreated callback,
// visibility-driven refreshes and resume re-pointing all funnel
// through here, so concurrent or repeated triggers for the same
// master target can never issue a second Target.createTarget on
// the same follower — late callers join the in-flight creation
// and share its result. The mapping is established only after
// the new page reports ready (WaitReady). A failed creation
// leaves NO mapping and removes its in-flight entry, recording
// the failure on the follower (creation failure: unconfirmed;
// readiness failure: failed), so a later trigger may retry.
func (sess *syncSession) ensureFollowerTarget(worker *followerWorker, masterTarget, url string) (string, error) {
	if ft, ok := worker.mappedTarget(masterTarget); ok {
		return ft, nil
	}
	key := targetCreationKey{followerID: worker.instanceID, masterTarget: masterTarget}
	sess.createMu.Lock()
	if sess.creating == nil {
		sess.creating = make(map[targetCreationKey]*targetCreation)
	}
	if call, inFlight := sess.creating[key]; inFlight {
		sess.createMu.Unlock()
		<-call.done
		return call.followerTarget, call.err
	}
	call := &targetCreation{done: make(chan struct{})}
	sess.creating[key] = call
	sess.createMu.Unlock()

	if url == "" {
		url = "about:blank"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ft, err := worker.createTarget(ctx, url)
	switch {
	case err != nil:
		worker.setUnconfirmed("无法为跟随实例创建对应页面：" + err.Error())
	case worker.tracker == nil:
		err = fmt.Errorf("跟随实例页面跟踪未就绪")
		worker.markFailed("跟随页面接入失败：" + err.Error())
	default:
		// Wait until the new page's session and tracking are
		// fully attached before the mapping becomes usable;
		// the createTarget result alone is not readiness.
		if werr := worker.tracker.WaitReady(ctx, ft); werr != nil {
			err = werr
			worker.markFailed("跟随页面接入失败：" + werr.Error())
		} else {
			worker.mapTarget(masterTarget, ft)
			call.followerTarget = ft
		}
	}
	call.err = err
	sess.createMu.Lock()
	delete(sess.creating, key)
	sess.createMu.Unlock()
	close(call.done)
	if err != nil {
		return "", err
	}
	return call.followerTarget, nil
}

// repointFollowers re-points every follower at the master's
// current active target after a resume: followers already
// pointing at the mapped page are left alone; followers whose
// pointing was skipped while paused (their bookkeeping followed
// the master, but no action was dispatched) are caught up. It
// never replays paused-period events — it only establishes the
// current pointing.
func (sess *syncSession) repointFollowers() {
	tracker := sess.tracker()
	if tracker == nil {
		return
	}
	sess.mu.Lock()
	targetID, known := sess.masterActive, sess.activeKnown
	sess.mu.Unlock()
	if !known {
		return
	}
	view := tracker.Pages()[targetID]
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		if worker == nil {
			continue
		}
		if ft, mapped := worker.mappedTarget(targetID); mapped {
			worker.mu.Lock()
			pointing := worker.activeTarget == ft
			worker.mu.Unlock()
			if pointing {
				continue
			}
			worker.setActiveTarget(ft)
			worker.enqueueControl(syncAction{kind: actionActivate, target: ft})
			continue
		}
		sess.pointFollower(worker, targetID, view.URL)
	}
}

func (sess *syncSession) onMasterNavigate(targetID, url string, reload bool) {
	if sess.paused() {
		return // navigation sync is part of dispatch: paused means stopped
	}
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		if worker == nil {
			continue
		}
		ft, ok := worker.mappedTarget(targetID)
		if !ok {
			continue
		}
		if reload {
			worker.enqueueControl(syncAction{kind: actionReload, target: ft})
		} else {
			worker.enqueueControl(syncAction{kind: actionNavigate, target: ft, url: url})
		}
	}
}

func (sess *syncSession) onMasterTargetCreated(targetID string) {
	go func() {
		tracker := sess.tracker()
		if sess.tornDown.Load() || tracker == nil {
			return
		}
		if sess.paused() {
			return // tab creation is dispatch too: paused means stopped
		}
		url := "about:blank"
		if view, ok := tracker.Pages()[targetID]; ok && view.URL != "" {
			url = view.URL
		}
		for _, id := range sess.order {
			worker := sess.followerByID(id)
			if worker == nil {
				continue
			}
			// The shared single-flight creation: if a
			// visibility-driven refresh is already creating (or
			// has created) this target's follower page, this
			// joins it instead of creating a duplicate.
			_, _ = sess.ensureFollowerTarget(worker, targetID, url)
		}
	}()
}

func (sess *syncSession) onMasterTargetDestroyed(targetID string) {
	sess.mu.Lock()
	if targetID == sess.masterActive {
		sess.activeKnown = false
		sess.masterActive = ""
	}
	paused := sess.phase == syncPhasePaused
	sess.mu.Unlock()
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		if worker == nil {
			continue
		}
		if ft, ok := worker.unmapTarget(targetID); ok {
			if paused {
				// Bookkeeping follows the master, but the close
				// itself is dispatch: while paused nothing is
				// executed on followers and nothing is replayed.
				continue
			}
			worker.enqueueControl(syncAction{kind: actionClose, target: ft})
		}
	}
}

// followerCallbacks builds the page callbacks for one follower
// connection: target destruction and visibility changes (the
// recovery signal for an unconfirmed follower).
func (sess *syncSession) followerCallbacks(instanceID string) pageCallbacks {
	return pageCallbacks{
		OnTargetDestroyed: sess.onFollowerTargetDestroyed(instanceID),
		OnVisibility:      sess.onFollowerVisibility(instanceID),
	}
}

func (sess *syncSession) onFollowerVisibility(instanceID string) func(string) {
	return func(string) {
		go sess.refreshFollowerActive(instanceID)
	}
}

// refreshFollowerActive re-confirms one follower's active page
// from its tracker's current visibility state. A follower whose
// active page could not be confirmed at start (or became
// ambiguous mid-session) recovers to ready once exactly one of
// its pages reports visible again AND that page is the one
// mapped to the master's current active page — identity is
// never guessed from titles or URLs. A failed follower stays
// failed: real execution errors are never auto-masked.
func (sess *syncSession) refreshFollowerActive(instanceID string) {
	if !sess.live.Load() || sess.tornDown.Load() {
		return // setup still in progress; recovery waits for live
	}
	worker := sess.followerByID(instanceID)
	if worker == nil || worker.tracker == nil {
		return
	}
	state, _, _ := worker.snapshot()
	if state != followerUnconfirmed {
		// Recovery applies to unconfirmed followers only: a
		// ready follower's pointing is owned by the session's
		// master-side mapping flow, and a failed follower stays
		// failed — real execution errors are never auto-masked.
		return
	}
	active, _, ok := worker.tracker.ActiveTarget()
	if !ok {
		worker.setUnconfirmed("无法确认跟随实例的活动页面")
		return
	}
	sess.mu.Lock()
	masterActive, known := sess.masterActive, sess.activeKnown
	masterW, masterH := sess.masterW, sess.masterH
	sess.mu.Unlock()
	if !known {
		worker.setUnconfirmed("主实例的活动页面尚未确认")
		return
	}
	mapped, isMapped := worker.mappedTarget(masterActive)
	if !isMapped {
		// No mapping yet (the follower was unconfirmed when the
		// session started): establish the initial pairing by the
		// same rule the session start uses — the follower's
		// unique visible page pairs with the master's current
		// active page. This is an explicit runtime mapping built
		// from session state, never from titles or URLs. A page
		// already mapped to another master target is never
		// stolen.
		worker.mu.Lock()
		for _, ft := range worker.targetMap {
			if ft == active {
				worker.mu.Unlock()
				worker.setUnconfirmed("跟随实例的活动页面已映射到其他页面")
				return
			}
		}
		worker.mu.Unlock()
		worker.mapTarget(masterActive, active)
		mapped = active
	}
	if mapped != active {
		// The follower's visible page is not the page mapped to
		// the master's active page; without an explicit mapping
		// its identity must not be guessed.
		worker.setUnconfirmed("跟随实例的活动页面尚未与主实例建立映射")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w, h, err := worker.tracker.Viewport(ctx, active); err == nil {
		worker.mu.Lock()
		worker.viewportW, worker.viewportH = w, h
		worker.masterW, worker.masterH = masterW, masterH
		worker.mu.Unlock()
	}
	worker.setActiveTarget(active)
	worker.setReady()
}

func (sess *syncSession) onFollowerTargetDestroyed(instanceID string) func(string) {
	return func(targetID string) {
		worker := sess.followerByID(instanceID)
		if worker == nil {
			return
		}
		worker.mu.Lock()
		for masterTarget, ft := range worker.targetMap {
			if ft == targetID {
				delete(worker.targetMap, masterTarget)
				worker.mu.Unlock()
				worker.markFailed("跟随实例的页面已关闭")
				sess.service.recordSessionEvent("跟随实例页面关闭", logging.StatusFailed, instanceID)
				return
			}
		}
		worker.mu.Unlock()
	}
}

// onMasterDisconnected ends the session when the master's
// connection drops: followers are released and closed, and the
// session returns to idle. Browsers themselves are never closed.
func (s *SyncService) onMasterDisconnected(sess *syncSession) {
	s.mu.Lock()
	if s.session != sess {
		s.mu.Unlock()
		return
	}
	s.session = nil
	s.mu.Unlock()
	sess.teardown()
	s.recordSessionEvent("主实例连接断开，同步已结束", logging.StatusFailed, sess.masterID)
}

// teardown releases every follower's input state (including
// failed followers), closes connections and marks the session
// gone. It never closes a browser or touches a profile.
func (sess *syncSession) teardown() {
	if !sess.tornDown.CompareAndSwap(false, true) {
		return
	}
	if sess.mirrorStop != nil {
		close(sess.mirrorStop)
	}
	for _, id := range sess.order {
		if worker := sess.followerByID(id); worker != nil {
			worker.stop()
			if worker.client != nil {
				_ = worker.client.Close()
			}
		}
	}
	if sess.masterClient != nil {
		_ = sess.masterClient.Close()
	}
}

// PauseSync halts ALL sync dispatch: pending actions are
// dropped, pressed state is released, and while paused the
// session handlers dispatch nothing at all — no page input, no
// navigation, no tab creation/closure/switching, no window
// mirroring. Nothing observed during the pause is replayed on
// resume; resume re-confirms the current active pages instead.
func (s *SyncService) PauseSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "暂停同步", "", "")
	err := s.withSession(func(sess *syncSession) error {
		sess.mu.Lock()
		if sess.phase != syncPhaseRunning {
			sess.mu.Unlock()
			return fmt.Errorf("同步未在进行中")
		}
		sess.phase = syncPhasePaused
		sess.mu.Unlock()
		for _, id := range sess.order {
			worker := sess.followerByID(id)
			worker.clearPending()
			worker.releaseAsync()
		}
		return nil
	})
	finishOp(op, err)
	if err == nil {
		s.recordSessionEvent("暂停同步", logging.StatusInfo, "")
	}
	return err
}

// ResumeSync resumes dispatch with empty queues: the master's
// current active page and every follower's mapping are
// re-confirmed first, so only events produced after the resume
// are ever dispatched.
func (s *SyncService) ResumeSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "恢复同步", "", "")
	var resumed *syncSession
	err := s.withSession(func(sess *syncSession) error {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if sess.phase != syncPhasePaused {
			return fmt.Errorf("同步未处于暂停状态")
		}
		sess.phase = syncPhaseRunning
		resumed = sess
		return nil
	})
	if err == nil && resumed != nil {
		resumed.refreshMasterActive()
		resumed.repointFollowers()
		for _, id := range resumed.order {
			resumed.refreshFollowerActive(id)
		}
	}
	finishOp(op, err)
	if err == nil {
		s.recordSessionEvent("恢复同步", logging.StatusInfo, "")
	}
	return err
}

// StopSync ends the session entirely.
func (s *SyncService) StopSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "停止同步", "", "")
	s.mu.Lock()
	sess := s.session
	s.session = nil
	s.mu.Unlock()
	if sess == nil {
		err := fmt.Errorf("当前没有进行中的同步会话")
		finishOp(op, err)
		return err
	}
	sess.teardown()
	finishOp(op, nil)
	s.recordSessionEvent("停止同步", logging.StatusSuccess, sess.masterID)
	return nil
}

func (s *SyncService) withSession(fn func(*syncSession) error) error {
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		return fmt.Errorf("当前没有进行中的同步会话")
	}
	return fn(sess)
}

// GetSyncState reports the session state for the sync page.
func (s *SyncService) GetSyncState() SyncState {
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		return SyncState{Phase: syncPhaseIdle, Followers: []SyncFollowerState{}}
	}
	sess.mu.Lock()
	state := SyncState{
		Phase:           sess.phase,
		MasterID:        sess.masterID,
		MasterConnected: sess.masterClient != nil && sess.masterClient.Err() == nil,
		MasterPageReady: sess.activeKnown,
		Followers:       []SyncFollowerState{},
	}
	sess.mu.Unlock()
	for _, id := range sess.order {
		worker := sess.followerByID(id)
		fState, reason, pageReady := worker.snapshot()
		state.Followers = append(state.Followers, SyncFollowerState{
			InstanceID: id,
			Connected:  worker.client != nil && worker.client.Err() == nil,
			PageReady:  pageReady,
			State:      fState,
			Reason:     reason,
		})
	}
	return state
}

// recordSessionEvent writes one runtime fact to the unified
// recording system. It carries instance IDs and outcomes only —
// never URLs, page content or input.
func (s *SyncService) recordSessionEvent(action, status, targetID string) {
	if s.Auditor == nil {
		return
	}
	s.Auditor.RecordEvent(logging.Event{
		EventType:  logging.EventTypeLifecycle,
		Category:   logging.EventCategoryRuntime,
		Module:     "多开同步",
		Action:     action,
		TargetType: "instance",
		TargetID:   targetID,
		Trigger:    "system",
		Status:     status,
	})
}

// windowForInstance resolves an instance's top-level window
// handle from its process ID.
func (s *SyncService) windowForInstance(instanceID string) (uintptr, error) {
	pid, ok := s.instances.instanceProcessID(instanceID)
	if !ok {
		return 0, fmt.Errorf("实例未在运行")
	}
	return winmgr.WindowForPID(pid)
}

// runWindowMirror polls the master window's placement and
// mirrors changes (move, resize, maximize, restore) to every
// follower window. Geometry changes are mirrored from the
// master's ACTUAL screen rectangle (GetWindowRect) through the
// non-activating SetWindowPos path; a show-state change
// (maximize/restore) goes through full placement application,
// for which no absolute foreground guarantee is claimed (see
// winmgr.MirrorPlacement). Minimizing the master is NOT
// mirrored (documented boundary: a minimized follower could
// not be restored by the user through sync, and hiding
// follower windows unprompted would be surprising). Mirroring
// pauses with the session and stops with it; it is
// independent of page input dispatch.
func (sess *syncSession) runWindowMirror() {
	if sess.masterHWND == 0 || len(sess.followerHWNDs) == 0 {
		return
	}
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-sess.mirrorStop:
			return
		case <-ticker.C:
		}
		if sess.tornDown.Load() || sess.paused() {
			continue
		}
		placement, err := winmgr.GetPlacement(sess.masterHWND)
		if err != nil {
			continue
		}
		screen, err := winmgr.GetWindowRect(sess.masterHWND)
		if err != nil {
			continue
		}
		sess.mu.Lock()
		changed := sess.lastMasterPl == nil || !winmgr.PlacementsEqual(*sess.lastMasterPl, placement) ||
			sess.lastMasterScreen == nil || *sess.lastMasterScreen != screen
		if changed {
			p := placement
			sess.lastMasterPl = &p
			r := screen
			sess.lastMasterScreen = &r
		}
		sess.mu.Unlock()
		if !changed {
			continue
		}
		for _, hwnd := range sess.followerHWNDs {
			// Best-effort per window: one window's failure never
			// blocks the others, and mirroring failures do not
			// fail the page sync session.
			_, _ = winmgr.MirrorPlacement(hwnd, placement.ShowCmd, screen, placement)
		}
	}
}

// ListSyncMonitors returns the displays available for arranging.
func (s *SyncService) ListSyncMonitors() ([]SyncMonitor, error) {
	monitors, err := winmgr.Monitors()
	if err != nil {
		return nil, err
	}
	out := make([]SyncMonitor, 0, len(monitors))
	for _, m := range monitors {
		out = append(out, SyncMonitor{
			Index: m.Index, Width: m.WorkArea.Width, Height: m.WorkArea.Height,
			Scale: m.Scale, Primary: m.Primary,
		})
	}
	return out, nil
}

// ArrangeWindows tiles every running instance's window across one
// monitor's work area, master first when a session is active.
// Pre-arrange placements are saved for RestoreWindows.
func (s *SyncService) ArrangeWindows(operationID string, monitorIndex int) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "自动排列窗口", "", "")
	err := s.arrangeWindows(monitorIndex)
	finishOp(op, err)
	return err
}

func (s *SyncService) arrangeWindows(monitorIndex int) error {
	monitors, err := winmgr.Monitors()
	if err != nil {
		return err
	}
	var monitor *winmgr.Monitor
	for i := range monitors {
		if monitors[i].Index == monitorIndex {
			monitor = &monitors[i]
			break
		}
	}
	if monitor == nil {
		return fmt.Errorf("所选显示器不存在")
	}
	list, err := s.instances.ListInstances()
	if err != nil {
		return err
	}
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	ordered := make([]model.Instance, 0, len(list))
	if sess != nil {
		for _, inst := range list {
			if inst.ID == sess.masterID && inst.Status == model.StatusRunning {
				ordered = append(ordered, inst)
			}
		}
	}
	for _, inst := range list {
		if inst.Status != model.StatusRunning {
			continue
		}
		if sess != nil && inst.ID == sess.masterID {
			continue
		}
		ordered = append(ordered, inst)
	}
	type target struct {
		id   string
		hwnd uintptr
	}
	targets := make([]target, 0, len(ordered))
	for _, inst := range ordered {
		pid, ok := s.instances.instanceProcessID(inst.ID)
		if !ok {
			continue
		}
		hwnd, werr := winmgr.WindowForPID(pid)
		if werr != nil {
			continue
		}
		targets = append(targets, target{id: inst.ID, hwnd: hwnd})
	}
	if len(targets) == 0 {
		return fmt.Errorf("没有找到可排列的浏览器窗口")
	}
	rects := winmgr.GridRects(monitor.WorkArea, len(targets))
	s.mu.Lock()
	for i, tg := range targets {
		if _, saved := s.savedPlacements[tg.id]; !saved {
			if placement, perr := winmgr.GetPlacement(tg.hwnd); perr == nil {
				s.savedPlacements[tg.id] = savedPlacement{hwnd: tg.hwnd, placement: placement}
			}
		}
		_ = i
	}
	s.mu.Unlock()
	var firstErr error
	for i, tg := range targets {
		// Arranging is a geometry operation: PlaceWindow moves
		// and resizes a normal-state window through the
		// non-activating SetWindowPos path; only a window that
		// first needs a show-state change (maximized/minimized
		// back to normal) goes through full placement restore,
		// whose foreground behaviour is a documented limitation
		// rather than an absolute guarantee.
		if serr := winmgr.PlaceWindow(tg.hwnd, rects[i]); serr != nil && firstErr == nil {
			firstErr = serr
		}
	}
	return firstErr
}

// setWindowPlacement applies one saved placement during a
// restore. It is a package variable so tests can script
// per-window outcomes; production always uses
// winmgr.SetPlacement.
var setWindowPlacement = winmgr.SetPlacement

// RestoreWindows returns arranged windows to the placements
// saved before the last arrange. Records are consumed PER
// WINDOW: a window that restores successfully has its record
// removed; a window whose restore fails KEEPS its original
// saved placement, so the user can retry restoring it later.
// One window's failure never stops the others from being
// attempted, and when every window succeeds the records are
// fully cleared.
func (s *SyncService) RestoreWindows(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "恢复窗口", "", "")
	s.mu.Lock()
	saved := make(map[string]savedPlacement, len(s.savedPlacements))
	for id, sp := range s.savedPlacements {
		saved[id] = sp
	}
	s.mu.Unlock()
	var err error
	if len(saved) == 0 {
		err = fmt.Errorf("没有可恢复的窗口布局")
	} else {
		for id, sp := range saved {
			if serr := setWindowPlacement(sp.hwnd, sp.placement); serr != nil {
				if err == nil {
					err = serr
				}
				continue // record retained for a later retry
			}
			s.mu.Lock()
			delete(s.savedPlacements, id)
			s.mu.Unlock()
		}
	}
	finishOp(op, err)
	return err
}
