package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/wininput"
)

// Stage 10 multi-instance sync. One master instance is operated
// by the user; DeBrowser mirrors the operations to the follower
// instances. Page-level input travels over each instance's own
// loopback CDP connection; browser-chrome input is mirrored with
// native Windows capture/injection. Sync moves ONLY user input —
// never profile data (see the privacy rules in the docs).
//
// The session state is deliberately small and memory-only: a
// master, a set of follower peers, and a lifecycle state. No
// queues, schedulers or persistent state are involved.

// Sync lifecycle states reported to the frontend.
const (
	SyncStateIdle   = "idle"
	SyncStateActive = "active"
	SyncStatePaused = "paused"
)

// SyncInstanceInfo is one row of the sync page's instance list.
type SyncInstanceInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	KernelName    string `json:"kernelName"`
	Status        string `json:"status"`
	CDPAvailable  bool   `json:"cdpAvailable"`
	CDPReason     string `json:"cdpReason"`
	Role          string `json:"role"` // "", "master", "follower"
	Failed        bool   `json:"failed"`
	FailureReason string `json:"failureReason"`
}

// SyncSessionState is the sync page's control/status snapshot.
type SyncSessionState struct {
	State     string             `json:"state"`
	MasterID  string             `json:"masterId"`
	Followers []SyncInstanceInfo `json:"followers"`
	LastError string             `json:"lastError"`
}

// SyncMonitor is one display offered for window arrangement.
type SyncMonitor struct {
	Index        int  `json:"index"`
	Primary      bool `json:"primary"`
	ScalePercent int  `json:"scalePercent"`
	WorkWidth    int  `json:"workWidth"`
	WorkHeight   int  `json:"workHeight"`
}

// ArrangeFailure reports one instance whose window could not be
// placed.
type ArrangeFailure struct {
	InstanceID string `json:"instanceId"`
	Reason     string `json:"reason"`
}

// ArrangeResult is the outcome of an arrange operation.
type ArrangeResult struct {
	Placed   []string         `json:"placed"`
	Failures []ArrangeFailure `json:"failures"`
}

// syncPeer is one sync participant's control channel.
type syncPeer struct {
	id     string
	client *cdp.Client

	mu        sync.Mutex
	targetID  string // current page target
	sessionID string // session of the current target
	cssW      float64
	cssH      float64
	failed    bool
	reason    string
	// targetByMaster maps a master target id to this peer's own
	// target id for tabs opened during the session.
	targetByMaster map[string]string
}

func (p *syncPeer) markFailed(reason string) {
	p.mu.Lock()
	p.failed = true
	p.reason = reason
	p.mu.Unlock()
}

// SyncService implements the multi-instance sync bindings.
type SyncService struct {
	instances *InstanceService
	log       *slog.Logger

	// Auditor, when set, receives structured operation records
	// for sync lifecycle actions (never input content).
	Auditor *logging.System

	mu        sync.Mutex
	state     string
	masterID  string
	peers     map[string]*syncPeer // includes the master
	lastError string

	events   chan wininput.Event
	stopDisp context.CancelFunc

	// Window arrangement session memory (never persisted).
	savedRects map[string]wininput.Rect
	savedHWND  map[string]uintptr
}

// NewSyncService builds the sync service on top of the instance
// runtime.
func NewSyncService(instances *InstanceService, logger *slog.Logger) *SyncService {
	if logger == nil {
		logger = slog.Default()
	}
	return &SyncService{
		instances:  instances,
		log:        logger,
		state:      SyncStateIdle,
		peers:      map[string]*syncPeer{},
		savedRects: map[string]wininput.Rect{},
		savedHWND:  map[string]uintptr{},
	}
}

// cdpAvailability explains whether an instance can be controlled
// over CDP right now, and if not, why not.
func (s *SyncService) cdpAvailability(inst model.Instance) (bool, string) {
	if inst.Status != model.StatusRunning {
		return false, "实例未在运行"
	}
	if s.instances.instanceCDPPort(inst.ID) == 0 {
		return false, "该实例由旧版本启动，暂不支持同步，请关闭后重新启动"
	}
	return true, ""
}

// ListSyncInstances returns every instance with its runtime
// status, CDP availability and current sync role.
func (s *SyncService) ListSyncInstances() ([]SyncInstanceInfo, error) {
	list, err := s.instances.ListInstances()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SyncInstanceInfo, 0, len(list))
	for _, inst := range list {
		info := SyncInstanceInfo{
			ID: inst.ID, Name: inst.Name, KernelName: inst.KernelName,
			Status: string(inst.Status),
		}
		info.CDPAvailable, info.CDPReason = s.cdpAvailability(inst)
		if s.state != SyncStateIdle {
			if inst.ID == s.masterID {
				info.Role = "master"
			} else if p, ok := s.peers[inst.ID]; ok {
				info.Role = "follower"
				p.mu.Lock()
				info.Failed, info.FailureReason = p.failed, p.reason
				p.mu.Unlock()
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// GetSyncState returns the current session snapshot.
func (s *SyncService) GetSyncState() SyncSessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SyncSessionState{State: s.state, MasterID: s.masterID, LastError: s.lastError}
	for id, p := range s.peers {
		if id == s.masterID {
			continue
		}
		info := SyncInstanceInfo{ID: id, Role: "follower"}
		p.mu.Lock()
		info.Failed, info.FailureReason = p.failed, p.reason
		p.mu.Unlock()
		st.Followers = append(st.Followers, info)
	}
	return st
}

// StartSync begins a sync session: exactly one master and at
// least one follower, every participant confirmed running with a
// working CDP connection. Any unmet precondition fails the whole
// start — a session is never reported as running when it is not.
func (s *SyncService) StartSync(operationID string, masterID string, followerIDs []string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.start", "instance", masterID)
	err := s.startSync(masterID, followerIDs)
	finishOp(op, err)
	return err
}

func (s *SyncService) startSync(masterID string, followerIDs []string) error {
	s.mu.Lock()
	if s.state != SyncStateIdle {
		s.mu.Unlock()
		return errors.New("同步已经在进行中")
	}
	s.mu.Unlock()
	if masterID == "" {
		return errors.New("请选择主实例")
	}
	if len(followerIDs) == 0 {
		return errors.New("至少需要一个跟随实例")
	}
	seen := map[string]bool{masterID: true}
	for _, id := range followerIDs {
		if seen[id] {
			return errors.New("主实例不能同时作为跟随实例，实例也不能重复加入")
		}
		seen[id] = true
	}

	list, err := s.instances.ListInstances()
	if err != nil {
		return err
	}
	byID := map[string]model.Instance{}
	for _, inst := range list {
		byID[inst.ID] = inst
	}

	// Every precondition is checked for ALL participants before
	// any connection is opened: a start that cannot succeed must
	// fail on the unmet condition, not on whichever connection
	// happened to be attempted first.
	participantIDs := append([]string{masterID}, followerIDs...)
	for _, id := range participantIDs {
		inst, ok := byID[id]
		if !ok {
			return fmt.Errorf("实例不存在：%s", id)
		}
		if avail, reason := s.cdpAvailability(inst); !avail {
			return fmt.Errorf("实例 %s 无法参与同步：%s", inst.Name, reason)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peers := map[string]*syncPeer{}
	cleanup := func() {
		for _, p := range peers {
			p.client.Close()
		}
	}
	for _, id := range participantIDs {
		inst := byID[id]
		port := s.instances.instanceCDPPort(id)
		client, err := cdp.Dial(ctx, port)
		if err != nil {
			cleanup()
			return fmt.Errorf("实例 %s 的浏览器控制连接建立失败（可能不是 Chromium 内核或调试端口未就绪）：%w", inst.Name, err)
		}
		if _, err := client.BrowserVersion(ctx); err != nil {
			client.Close()
			cleanup()
			return fmt.Errorf("实例 %s 的浏览器不支持 CDP 同步：%w", inst.Name, err)
		}
		peer := &syncPeer{id: id, client: client, targetByMaster: map[string]string{}}
		if err := peer.attachToCurrentPage(ctx); err != nil {
			client.Close()
			cleanup()
			return fmt.Errorf("实例 %s 无法附加到当前页面：%w", inst.Name, err)
		}
		_ = client.EnableTargetEvents(ctx)
		peers[id] = peer
	}

	s.mu.Lock()
	s.peers = peers
	s.masterID = masterID
	s.state = SyncStateActive
	s.lastError = ""
	s.mu.Unlock()

	s.attachEventHandlers()
	if err := s.startDispatch(); err != nil {
		s.stopSyncInternal()
		return fmt.Errorf("启动输入捕获失败：%w", err)
	}
	// If the master's control connection dies, the session is
	// over: stop cleanly and surface the reason.
	go func() {
		<-peers[masterID].client.Done()
		s.mu.Lock()
		active := s.state != SyncStateIdle && s.masterID == masterID
		s.mu.Unlock()
		if active {
			s.stopSyncInternal()
			s.mu.Lock()
			s.lastError = "主实例的控制连接已断开，同步已停止"
			s.mu.Unlock()
		}
	}()
	s.log.Info("sync session started", "master", masterID, "followers", len(followerIDs))
	return nil
}

// PauseSync stops dispatching new operations to followers.
func (s *SyncService) PauseSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.pause", "instance", s.currentMaster())
	s.mu.Lock()
	if s.state != SyncStateActive {
		s.mu.Unlock()
		err := errors.New("同步未在进行中")
		finishOp(op, err)
		return err
	}
	s.state = SyncStatePaused
	s.mu.Unlock()
	s.releaseFollowerInputs()
	finishOp(op, nil)
	return nil
}

// ResumeSync resumes a paused session.
func (s *SyncService) ResumeSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.resume", "instance", s.currentMaster())
	s.mu.Lock()
	if s.state != SyncStatePaused {
		s.mu.Unlock()
		err := errors.New("同步未处于暂停状态")
		finishOp(op, err)
		return err
	}
	s.state = SyncStateActive
	s.mu.Unlock()
	finishOp(op, nil)
	return nil
}

// StopSync ends the session: input state is released, control
// connections close, temporary state is dropped. Browsers keep
// running.
func (s *SyncService) StopSync(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.stop", "instance", s.currentMaster())
	s.mu.Lock()
	wasIdle := s.state == SyncStateIdle
	s.mu.Unlock()
	if wasIdle {
		err := errors.New("同步未在进行中")
		finishOp(op, err)
		return err
	}
	s.stopSyncInternal()
	finishOp(op, nil)
	return nil
}

func (s *SyncService) currentMaster() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.masterID
}

// stopSyncInternal performs the teardown shared by StopSync and
// the master-disconnect path. It never touches the browsers
// themselves.
func (s *SyncService) stopSyncInternal() {
	s.releaseFollowerInputs()
	wininput.StopCapture()
	s.mu.Lock()
	if s.stopDisp != nil {
		s.stopDisp()
		s.stopDisp = nil
	}
	peers := s.peers
	s.peers = map[string]*syncPeer{}
	s.state = SyncStateIdle
	s.masterID = ""
	s.mu.Unlock()
	for _, p := range peers {
		p.client.Close()
	}
	s.log.Info("sync session stopped")
}

// ListSyncMonitors returns the displays available for window
// arrangement.
func (s *SyncService) ListSyncMonitors() ([]SyncMonitor, error) {
	monitors := wininput.ListMonitors()
	out := make([]SyncMonitor, 0, len(monitors))
	for i, m := range monitors {
		out = append(out, SyncMonitor{
			Index: i, Primary: m.Primary, ScalePercent: m.ScalePercent,
			WorkWidth: int(m.WorkRect.Width), WorkHeight: int(m.WorkRect.Height),
		})
	}
	return out, nil
}

// ArrangeSyncWindows lays out the given running instances'
// windows in a grid on the chosen monitor, remembering each
// window's previous rectangle for RestoreSyncWindows.
func (s *SyncService) ArrangeSyncWindows(operationID string, instanceIDs []string, monitorIndex int) (ArrangeResult, error) {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.arrange", "instance", "")
	result, err := s.arrange(instanceIDs, monitorIndex)
	finishOp(op, err)
	return result, err
}

func (s *SyncService) arrange(instanceIDs []string, monitorIndex int) (ArrangeResult, error) {
	var result ArrangeResult
	if len(instanceIDs) == 0 {
		return result, errors.New("请先选择参与排列的实例")
	}
	monitors := wininput.ListMonitors()
	if monitorIndex < 0 || monitorIndex >= len(monitors) {
		return result, errors.New("所选显示器不存在")
	}
	work := monitors[monitorIndex].WorkRect
	rects := wininput.GridRects(work, len(instanceIDs))

	s.mu.Lock()
	if s.savedRects == nil {
		s.savedRects = map[string]wininput.Rect{}
		s.savedHWND = map[string]uintptr{}
	}
	s.mu.Unlock()

	for i, id := range instanceIDs {
		pid, ok := s.instances.instanceProcessID(id)
		if !ok {
			result.Failures = append(result.Failures, ArrangeFailure{id, "实例未在运行"})
			continue
		}
		hwnd, ok := wininput.TopWindowForPID(uint32(pid))
		if !ok {
			result.Failures = append(result.Failures, ArrangeFailure{id, "未找到实例的浏览器窗口"})
			continue
		}
		if _, saved := s.savedHWND[id]; !saved {
			if r, ok := wininput.WindowPlacement(hwnd); ok {
				s.mu.Lock()
				s.savedRects[id] = r
				s.savedHWND[id] = hwnd
				s.mu.Unlock()
			}
		}
		if err := wininput.MoveWindow(hwnd, rects[i]); err != nil {
			result.Failures = append(result.Failures, ArrangeFailure{id, "窗口位置调整失败：" + err.Error()})
			continue
		}
		result.Placed = append(result.Placed, id)
	}
	return result, nil
}

// RestoreSyncWindows returns arranged windows to the rectangles
// recorded by the last arrange.
func (s *SyncService) RestoreSyncWindows(operationID string) error {
	op := beginOp(s.Auditor, operationID, "多开同步", "sync.restore_windows", "instance", "")
	err := s.restore()
	finishOp(op, err)
	return err
}

func (s *SyncService) restore() error {
	s.mu.Lock()
	saved := s.savedRects
	hwnds := s.savedHWND
	s.savedRects = map[string]wininput.Rect{}
	s.savedHWND = map[string]uintptr{}
	s.mu.Unlock()
	if len(saved) == 0 {
		return errors.New("没有可恢复的窗口布局")
	}
	var firstErr error
	for id, r := range saved {
		hwnd := hwnds[id]
		if hwnd == 0 {
			continue
		}
		if err := wininput.MoveWindow(hwnd, r); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
