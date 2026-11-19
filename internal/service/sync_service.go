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

	switchMu  sync.Mutex
	followers map[string]*followerWorker
	order     []string
	tornDown  atomic.Bool
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
		service:   s,
		phase:     syncPhaseRunning,
		masterID:  masterID,
		followers: make(map[string]*followerWorker),
		order:     append([]string(nil), followerIDs...),
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
	sess.masterTracker = masterTracker
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
		sess.followers[id] = worker
		client, tracker, derr := dialBrowser(ctx, followerPorts[id], pageCallbacks{
			OnTargetDestroyed: sess.onFollowerTargetDestroyed(id),
		})
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

	s.mu.Lock()
	if s.session != nil { // a concurrent start won the race
		s.mu.Unlock()
		sess.teardown()
		return fmt.Errorf("同步会话已在进行中")
	}
	s.session = sess
	s.mu.Unlock()

	go func() {
		<-masterClient.Done()
		sess.service.onMasterDisconnected(sess)
	}()

	s.recordSessionEvent("开始同步", logging.StatusSuccess, masterID)
	return nil
}

// --- master event handlers (called from tracker callbacks; they
// never block the CDP read path) ---

func (sess *syncSession) onMasterPageEvent(targetID string, ev PageEvent) {
	sess.mu.Lock()
	if sess.phase != syncPhaseRunning || !sess.activeKnown || targetID != sess.masterActive {
		sess.mu.Unlock()
		return
	}
	sess.mu.Unlock()
	for _, id := range sess.order {
		worker := sess.followers[id]
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
	if sess.tornDown.Load() || sess.masterTracker == nil {
		return
	}
	targetID, view, ok := sess.masterTracker.ActiveTarget()
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
	sess.mu.Unlock()
	if !changed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if w, h, err := sess.masterTracker.Viewport(ctx, targetID); err == nil {
		sess.mu.Lock()
		sess.masterW, sess.masterH = w, h
		sess.mu.Unlock()
	}
	for _, id := range sess.order {
		worker := sess.followers[id]
		if worker == nil {
			continue
		}
		worker.mu.Lock()
		worker.masterW, worker.masterH = sess.masterW, sess.masterH
		worker.mu.Unlock()
		if ft, mapped := worker.mappedTarget(targetID); mapped {
			worker.setActiveTarget(ft)
			worker.enqueueControl(syncAction{kind: actionActivate, target: ft})
			continue
		}
		url := view.URL
		if url == "" {
			url = "about:blank"
		}
		ft, err := worker.createTarget(ctx, url)
		if err != nil {
			worker.setUnconfirmed("无法为跟随实例创建对应页面：" + err.Error())
			continue
		}
		worker.mapTarget(targetID, ft)
		worker.setActiveTarget(ft)
		worker.enqueueControl(syncAction{kind: actionActivate, target: ft})
	}
}

func (sess *syncSession) onMasterNavigate(targetID, url string, reload bool) {
	for _, id := range sess.order {
		worker := sess.followers[id]
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
		if sess.tornDown.Load() || sess.masterTracker == nil {
			return
		}
		url := "about:blank"
		if view, ok := sess.masterTracker.Pages()[targetID]; ok && view.URL != "" {
			url = view.URL
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, id := range sess.order {
			worker := sess.followers[id]
			if worker == nil {
				continue
			}
			ft, err := worker.createTarget(ctx, url)
			if err != nil {
				worker.setUnconfirmed("无法为跟随实例创建对应页面：" + err.Error())
				continue
			}
			worker.mapTarget(targetID, ft)
		}
	}()
}

func (sess *syncSession) onMasterTargetDestroyed(targetID string) {
	sess.mu.Lock()
	if targetID == sess.masterActive {
		sess.activeKnown = false
		sess.masterActive = ""
	}
	sess.mu.Unlock()
	for _, id := range sess.order {
		worker := sess.followers[id]
		if worker == nil {
			continue
		}
		if ft, ok := worker.unmapTarget(targetID); ok {
			worker.enqueueControl(syncAction{kind: actionClose, target: ft})
		}
	}
}

func (sess *syncSession) onFollowerTargetDestroyed(instanceID string) func(string) {
	return func(targetID string) {
		worker := sess.followers[instanceID]
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
	for _, id := range sess.order {
		if worker := sess.followers[id]; worker != nil {
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

// PauseSync halts input dispatch: pending input is dropped and
// pressed state is released; nothing is replayed on resume.
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
			worker := sess.followers[id]
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

// ResumeSync resumes input dispatch with empty queues.
func (s *SyncService) ResumeSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "恢复同步", "", "")
	err := s.withSession(func(sess *syncSession) error {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		if sess.phase != syncPhasePaused {
			return fmt.Errorf("同步未处于暂停状态")
		}
		sess.phase = syncPhaseRunning
		return nil
	})
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
		worker := sess.followers[id]
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

// --- window management (independent of any session) ---

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
		if serr := winmgr.SetPlacement(tg.hwnd, winmgr.Placement{Rect: rects[i]}); serr != nil && firstErr == nil {
			firstErr = serr
		}
	}
	return firstErr
}

// RestoreWindows returns arranged windows to the placements saved
// before the last arrange.
func (s *SyncService) RestoreWindows(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "恢复窗口", "", "")
	s.mu.Lock()
	saved := s.savedPlacements
	s.savedPlacements = make(map[string]savedPlacement)
	s.mu.Unlock()
	var err error
	if len(saved) == 0 {
		err = fmt.Errorf("没有可恢复的窗口布局")
	} else {
		for _, sp := range saved {
			if serr := winmgr.SetPlacement(sp.hwnd, sp.placement); serr != nil && err == nil {
				err = serr
			}
		}
	}
	finishOp(op, err)
	return err
}
