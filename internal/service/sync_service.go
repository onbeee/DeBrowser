package service

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/uia"
	"github.com/onbeee/DeBrowser/internal/winmgr"
)

// SyncService implements Stage 10 v2 multi-instance sync.
//
// Redesign summary (replacing PR #17 wholesale):
//   - Page content input travels over each instance's own CDP
//     connection, dispatched by a dedicated per-follower worker
//     with its own bounded queue — followers can never block
//     each other or the input hook.
//   - Browser chrome is handled SEMANTICALLY: tabs and
//     navigation through CDP target/page commands, per-instance
//     history through each follower's own navigation history,
//     and UI Automation for capability discovery, tab selection
//     detection and control invocation. There is NO SendInput,
//     no PostMessage input simulation, no foreground switching
//     and no coordinate guessing anywhere in this design.
//   - Window geometry uses native window management with
//     no-activation flags only.
//   - Anything that cannot be done reliably is reported per
//     follower with a reason — never faked, never silently
//     dropped.
//
// Sessions and window-arrangement baselines live in memory only.

type SyncInstanceOption struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Running         bool   `json:"running"`
	CDPSupported    bool   `json:"cdpSupported"`
	UnsupportedNote string `json:"unsupportedNote,omitempty"`
}

type SyncCapabilitiesInfo struct {
	Available  bool `json:"available"`
	AddressBar bool `json:"addressBar"`
	TabStrip   bool `json:"tabStrip"`
	MenuButton bool `json:"menuButton"`
}

type SyncFollowerState struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	Status string               `json:"status"` // active | failed
	Reason string               `json:"reason,omitempty"`
	Caps   SyncCapabilitiesInfo `json:"caps"`
}

type SyncState struct {
	Phase       string               `json:"phase"` // idle | running | paused
	MasterID    string               `json:"masterId,omitempty"`
	MasterName  string               `json:"masterName,omitempty"`
	MasterCaps  SyncCapabilitiesInfo `json:"masterCaps"`
	Followers   []SyncFollowerState  `json:"followers"`
	UISkipped   int64                `json:"uiSkipped"`
	HasArranged bool                 `json:"hasArranged"`
}

type SyncMonitorInfo struct {
	Index   int     `json:"index"`
	Primary bool    `json:"primary"`
	Width   int32   `json:"width"`
	Height  int32   `json:"height"`
	Scale   float64 `json:"scale"`
}

type SyncWindowResult struct {
	InstanceID string `json:"instanceId"`
	Name       string `json:"name"`
	OK         bool   `json:"ok"`
	Reason     string `json:"reason,omitempty"`
}

type syncSession struct {
	masterID     string
	masterClient *cdp.Client
	masterPID    uint32

	mMu            sync.Mutex
	targets        []string          // master page targets, tab order
	sessions       map[string]string // targetId -> sessionId
	targetURLs     map[string]string
	currentTarget  string
	currentURL     string
	lastLoaderID   string
	histLen        int
	histIndex      int
	histEntryURL   string
	histInit       bool
	masterViewport struct{ w, h float64 }
	lastMasterTab  int
	winInit        bool
	lastRect       winmgr.Rect
	lastState      winmgr.WindowState

	workers map[string]*followerWorker
	order   []string
	caps    map[string]uia.Capabilities

	paused    atomic.Bool
	uiSkipped atomic.Int64
	focusCtx  atomic.Int32 // 0 = page content, 1 = browser chrome

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// onFollowerFail / master-gone audit hooks are set by the
	// service before goroutines start.
	svc *SyncService
}

type SyncService struct {
	instances *InstanceService
	Auditor   *logging.System

	mu       sync.Mutex
	session  *syncSession
	savedWin map[string]savedWindow
}

type savedWindow struct {
	hwnd  uintptr
	rect  winmgr.Rect
	state winmgr.WindowState
}

func NewSyncService(instances *InstanceService) *SyncService {
	return &SyncService{instances: instances, savedWin: make(map[string]savedWindow)}
}

func capsInfo(c uia.Capabilities) SyncCapabilitiesInfo {
	return SyncCapabilitiesInfo{
		Available: c.Available, AddressBar: c.AddressBar,
		TabStrip: c.TabStrip, MenuButton: c.MenuButton,
	}
}

// ListSyncInstances lists instances for the sync pickers.
func (s *SyncService) ListSyncInstances(operationID string) ([]SyncInstanceOption, error) {
	op := beginOp(s.Auditor, operationID, "sync", "list_instances", "sync", "")
	list, err := s.instances.store.ListInstances(context.Background())
	if err != nil {
		finishOp(op, err)
		return nil, err
	}
	out := make([]SyncInstanceOption, 0, len(list))
	for _, inst := range list {
		opt := SyncInstanceOption{ID: inst.ID, Name: inst.Name}
		if s.instances.effectiveStatus(inst) == model.StatusRunning {
			opt.Running = true
			if s.instances.instanceCDPPort(inst.ID) > 0 {
				opt.CDPSupported = true
			} else {
				opt.UnsupportedNote = "该实例不是以同步模式启动的，请关闭后重新启动再参与同步"
			}
		}
		out = append(out, opt)
	}
	finishOp(op, nil)
	return out, nil
}

func (s *SyncService) runningInstance(id string) (model.Instance, error) {
	inst, err := s.instances.store.GetInstance(context.Background(), id)
	if err != nil {
		return model.Instance{}, fmt.Errorf("实例不存在")
	}
	if s.instances.effectiveStatus(inst) != model.StatusRunning {
		return model.Instance{}, fmt.Errorf("实例「%s」未在运行", inst.Name)
	}
	if s.instances.instanceCDPPort(id) <= 0 {
		return model.Instance{}, fmt.Errorf("实例「%s」不是以同步模式启动的，请关闭后重新启动", inst.Name)
	}
	return inst, nil
}

// StartSync validates every participant, connects, probes UIA
// capabilities, and only then starts the session.
func (s *SyncService) StartSync(operationID, masterID string, followerIDs []string) error {
	op := beginOp(s.Auditor, operationID, "sync", "start", "sync", masterID)
	err := s.startSync(masterID, followerIDs)
	finishOp(op, err)
	return err
}

func (s *SyncService) startSync(masterID string, followerIDs []string) error {
	s.mu.Lock()
	if s.session != nil {
		s.mu.Unlock()
		return fmt.Errorf("已有同步会话在进行中，请先停止")
	}
	s.mu.Unlock()

	if len(followerIDs) == 0 {
		return fmt.Errorf("请至少选择一个跟随实例")
	}
	master, err := s.runningInstance(masterID)
	if err != nil {
		return err
	}
	seen := map[string]bool{masterID: true}
	followers := make([]model.Instance, 0, len(followerIDs))
	for _, fid := range followerIDs {
		if seen[fid] {
			return fmt.Errorf("主实例与跟随实例不能重复")
		}
		seen[fid] = true
		inst, err := s.runningInstance(fid)
		if err != nil {
			return err
		}
		followers = append(followers, inst)
	}

	ctx := context.Background()
	dialCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	masterClient, err := cdp.Dial(dialCtx, s.instances.instanceCDPPort(masterID))
	cancel()
	if err != nil {
		return fmt.Errorf("无法连接主实例「%s」的调试接口：%v", master.Name, err)
	}

	sess := &syncSession{
		masterID: masterID, masterClient: masterClient,
		masterPID:     uint32(s.instances.instanceProcessID(masterID)),
		sessions:      make(map[string]string),
		targetURLs:    make(map[string]string),
		workers:       make(map[string]*followerWorker),
		caps:          make(map[string]uia.Capabilities),
		lastMasterTab: -1,
		svc:           s,
	}
	sessCtx, sessCancel := context.WithCancel(ctx)
	sess.cancel = sessCancel

	failStart := func(err error) error {
		sessCancel()
		masterClient.Close()
		for _, w := range sess.workers {
			w.client.Close()
		}
		return err
	}

	if err := masterClient.EnableTargetEvents(ctx); err != nil {
		return failStart(fmt.Errorf("主实例调试接口初始化失败：%v", err))
	}
	masterTargets, err := sess.discoverTargets(masterClient)
	if err != nil {
		return failStart(fmt.Errorf("读取主实例标签失败：%v", err))
	}
	if len(masterTargets) == 0 {
		return failStart(fmt.Errorf("主实例没有可同步的标签页"))
	}

	// UIA capability probing (also yields the master's selected
	// tab index, which target lists alone cannot tell us).
	masterHWND := topHWND(sess.masterPID)
	if caps, err := uia.ProbeWindow(masterHWND); err == nil {
		sess.caps[masterID] = caps
	} else {
		sess.caps[masterID] = uia.Capabilities{SelectedTab: -1, Reason: "无法读取浏览器界面控件信息"}
	}
	activeIdx := 0
	if c := sess.caps[masterID]; c.SelectedTab >= 0 && c.SelectedTab < len(masterTargets) {
		activeIdx = c.SelectedTab
	}
	sess.lastMasterTab = activeIdx
	if err := sess.attachMasterTarget(masterTargets[activeIdx]); err != nil {
		return failStart(fmt.Errorf("连接主实例标签失败：%v", err))
	}
	sess.refreshMasterViewport()

	// Followers: connect and queue an init event on each worker.
	for _, finst := range followers {
		fid := finst.ID
		dctx, dcancel := context.WithTimeout(ctx, 8*time.Second)
		client, err := cdp.Dial(dctx, s.instances.instanceCDPPort(fid))
		dcancel()
		if err != nil {
			return failStart(fmt.Errorf("无法连接跟随实例「%s」的调试接口：%v", finst.Name, err))
		}
		if err := client.EnableTargetEvents(ctx); err != nil {
			client.Close()
			return failStart(fmt.Errorf("跟随实例「%s」调试接口初始化失败：%v", finst.Name, err))
		}
		ftargets, err := sess.discoverTargets(client)
		if err != nil || len(ftargets) == 0 {
			client.Close()
			return failStart(fmt.Errorf("读取跟随实例「%s」的标签失败", finst.Name))
		}
		w := newFollowerWorker(fid, client, func(id, reason string) {
			s.noteFollowerFailed(sess, id, reason)
		})
		sess.workers[fid] = w
		sess.order = append(sess.order, fid)
		if caps, err := uia.ProbeWindow(topHWND(uint32(s.instances.instanceProcessID(fid)))); err == nil {
			sess.caps[fid] = caps
		} else {
			sess.caps[fid] = uia.Capabilities{SelectedTab: -1, Reason: "无法读取浏览器界面控件信息"}
		}
		cur := activeIdx
		if cur >= len(ftargets) {
			cur = len(ftargets) - 1
		}
		w.queue.push(syncEvent{
			kind: evInit, targets: ftargets, index: cur,
			masterW: sess.masterViewport.w, masterH: sess.masterViewport.h,
		})
	}
	for _, w := range sess.workers {
		go w.Run()
	}

	masterClient.OnEvent(func(ev cdp.Event) { sess.onMasterEvent(ev) })
	sess.wg.Add(3)
	go sess.historyPoller(sessCtx)
	go sess.windowPoller(sessCtx)
	go sess.tabPoller(sessCtx)
	if err := sess.startCapture(); err != nil {
		sess.teardown()
		return fmt.Errorf("无法开始捕获主实例输入：%v", err)
	}
	go sess.watchMaster()

	s.mu.Lock()
	s.session = sess
	s.mu.Unlock()
	return nil
}

// discoverTargets returns the page targets of one browser in
// creation order.
func (s *syncSession) discoverTargets(client *cdp.Client) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	infos, err := client.Targets(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ti := range infos {
		if ti.Type == "page" {
			out = append(out, ti.TargetID)
		}
	}
	return out, nil
}

func topHWND(pid uint32) uintptr {
	if pid == 0 {
		return 0
	}
	if w, ok := winmgr.TopWindowForPID(pid); ok {
		return w.HWND
	}
	return 0
}

// PauseSync halts dispatch and releases all injected input.
func (s *SyncService) PauseSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "sync", "pause", "sync", "")
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		err := fmt.Errorf("当前没有进行中的同步会话")
		finishOp(op, err)
		return err
	}
	sess.paused.Store(true)
	for _, w := range sess.workers {
		w.queue.clearInput()
		w.Release()
	}
	finishOp(op, nil)
	return nil
}

// ResumeSync resumes dispatch; input from the pause is never
// replayed (queues were cleared and moves are not re-sent).
func (s *SyncService) ResumeSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "sync", "resume", "sync", "")
	s.mu.Lock()
	sess := s.session
	s.mu.Unlock()
	if sess == nil {
		err := fmt.Errorf("当前没有进行中的同步会话")
		finishOp(op, err)
		return err
	}
	sess.paused.Store(false)
	finishOp(op, nil)
	return nil
}

// StopSync ends the session: capture and dispatch stop, all
// follower input state is released, connections close. Browsers
// keep running; no profile is touched.
func (s *SyncService) StopSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "sync", "stop", "sync", "")
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
	return nil
}

// teardown releases and closes everything a session owns.
func (s *syncSession) teardown() {
	s.stopCapture()
	if s.cancel != nil {
		s.cancel()
	}
	for _, w := range s.workers {
		w.Stop()
	}
	for _, w := range s.workers {
		<-w.done
		w.client.Close()
	}
	s.masterClient.Close()
	s.wg.Wait()
}

// watchMaster ends the whole session when the master's CDP
// connection dies (browser closed or crashed).
func (s *syncSession) watchMaster() {
	<-s.masterClient.Done()
	s.svc.mu.Lock()
	if s.svc.session == s {
		s.svc.session = nil
		s.svc.mu.Unlock()
		s.teardown()
		if s.svc.Auditor != nil {
			s.svc.Auditor.RecordEvent(logging.Event{
				EventType: "runtime", Category: "runtime", Module: "sync",
				Action: "master_disconnected", TargetType: "instance", TargetID: s.masterID,
				Trigger: "system", Status: logging.StatusFailed,
				UserMessage: "主实例连接已断开，同步会话结束",
			})
		}
		return
	}
	s.svc.mu.Unlock()
}

func (s *SyncService) noteFollowerFailed(sess *syncSession, id, reason string) {
	if s.Auditor != nil {
		s.Auditor.RecordEvent(logging.Event{
			EventType: "runtime", Category: "runtime", Module: "sync",
			Action: "follower_failed", TargetType: "instance", TargetID: id,
			Trigger: "system", Status: logging.StatusFailed,
			UserMessage: "跟随实例同步失败：" + reason,
		})
	}
}

// GetSyncState reports the session for the sync page.
func (s *SyncService) GetSyncState(operationID string) (SyncState, error) {
	s.mu.Lock()
	sess := s.session
	arranged := len(s.savedWin) > 0
	s.mu.Unlock()
	st := SyncState{Phase: "idle", Followers: []SyncFollowerState{}, HasArranged: arranged}
	if sess == nil {
		return st, nil
	}
	st.Phase = "running"
	if sess.paused.Load() {
		st.Phase = "paused"
	}
	st.MasterID = sess.masterID
	st.UISkipped = sess.uiSkipped.Load()
	if inst, err := s.instances.store.GetInstance(context.Background(), sess.masterID); err == nil {
		st.MasterName = inst.Name
	}
	st.MasterCaps = capsInfo(sess.caps[sess.masterID])
	for _, fid := range sess.order {
		w := sess.workers[fid]
		fs := SyncFollowerState{ID: fid, Status: "active", Caps: capsInfo(sess.caps[fid])}
		if inst, err := s.instances.store.GetInstance(context.Background(), fid); err == nil {
			fs.Name = inst.Name
		}
		if failed, reason := w.isFailed(); failed {
			fs.Status = "failed"
			fs.Reason = reason
		}
		st.Followers = append(st.Followers, fs)
	}
	return st, nil
}

// ListSyncMonitors lists displays for the arrangement picker.
func (s *SyncService) ListSyncMonitors(operationID string) ([]SyncMonitorInfo, error) {
	op := beginOp(s.Auditor, operationID, "sync", "list_monitors", "sync", "")
	monitors, err := winmgr.ListMonitors()
	if err != nil {
		finishOp(op, err)
		return nil, fmt.Errorf("无法读取显示器信息")
	}
	out := make([]SyncMonitorInfo, 0, len(monitors))
	for i, m := range monitors {
		out = append(out, SyncMonitorInfo{
			Index: i, Primary: m.Primary,
			Width: m.Rect.Width, Height: m.Rect.Height, Scale: m.Scale,
		})
	}
	finishOp(op, nil)
	return out, nil
}

// ArrangeSyncWindows lays the given instances' windows out in a
// grid on one monitor. It works with or without a sync session.
// Original positions are remembered for RestoreSyncWindows.
func (s *SyncService) ArrangeSyncWindows(operationID string, monitorIndex int, instanceIDs []string) ([]SyncWindowResult, error) {
	op := beginOp(s.Auditor, operationID, "sync", "arrange_windows", "sync", "")
	results, err := s.arrange(monitorIndex, instanceIDs)
	finishOp(op, err)
	return results, err
}

func (s *SyncService) arrange(monitorIndex int, instanceIDs []string) ([]SyncWindowResult, error) {
	if len(instanceIDs) == 0 {
		return nil, fmt.Errorf("请选择要排列的实例")
	}
	monitors, err := winmgr.ListMonitors()
	if err != nil || len(monitors) == 0 {
		return nil, fmt.Errorf("无法读取显示器信息")
	}
	if monitorIndex < 0 || monitorIndex >= len(monitors) {
		monitorIndex = 0
	}
	area := monitors[monitorIndex].WorkArea
	cells := winmgr.GridRects(area, len(instanceIDs))
	results := make([]SyncWindowResult, 0, len(instanceIDs))
	for i, id := range instanceIDs {
		res := SyncWindowResult{InstanceID: id}
		if inst, err := s.instances.store.GetInstance(context.Background(), id); err == nil {
			res.Name = inst.Name
		}
		pid := uint32(s.instances.instanceProcessID(id))
		win, ok := winmgr.TopWindowForPID(pid)
		if pid == 0 || !ok {
			res.Reason = "实例未在运行或找不到窗口"
			results = append(results, res)
			continue
		}
		s.mu.Lock()
		if _, saved := s.savedWin[id]; !saved {
			s.savedWin[id] = savedWindow{hwnd: win.HWND, rect: win.Rect, state: win.State}
		}
		s.mu.Unlock()
		if err := winmgr.SetWindowRect(win.HWND, cells[i]); err != nil {
			res.Reason = "窗口排列失败"
		} else {
			res.OK = true
		}
		results = append(results, res)
	}
	return results, nil
}

// RestoreSyncWindows returns arranged windows to the positions
// saved before the first arrangement.
func (s *SyncService) RestoreSyncWindows(operationID string) ([]SyncWindowResult, error) {
	op := beginOp(s.Auditor, operationID, "sync", "restore_windows", "sync", "")
	s.mu.Lock()
	saved := s.savedWin
	s.savedWin = make(map[string]savedWindow)
	s.mu.Unlock()
	ids := make([]string, 0, len(saved))
	for id := range saved {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := make([]SyncWindowResult, 0, len(ids))
	for _, id := range ids {
		sw := saved[id]
		res := SyncWindowResult{InstanceID: id}
		if inst, err := s.instances.store.GetInstance(context.Background(), id); err == nil {
			res.Name = inst.Name
		}
		pid := uint32(s.instances.instanceProcessID(id))
		win, ok := winmgr.TopWindowForPID(pid)
		if pid == 0 || !ok {
			res.Reason = "实例未在运行或找不到窗口"
			results = append(results, res)
			continue
		}
		if err := winmgr.SetWindowRect(win.HWND, sw.rect); err != nil {
			res.Reason = "恢复窗口位置失败"
			results = append(results, res)
			continue
		}
		if sw.state != winmgr.StateNormal {
			_ = winmgr.SetWindowState(win.HWND, sw.state)
		}
		res.OK = true
		results = append(results, res)
	}
	if len(ids) == 0 {
		err := fmt.Errorf("没有可恢复的窗口位置")
		finishOp(op, err)
		return results, err
	}
	finishOp(op, nil)
	return results, nil
}

var _ = strings.TrimSpace
