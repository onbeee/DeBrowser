package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/wininput"
)

// attachToCurrentPage attaches the peer to its first page target
// and records the viewport size used for coordinate mapping.
func (p *syncPeer) attachToCurrentPage(ctx context.Context) error {
	targets, err := p.client.Targets(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if t.Type != "page" {
			continue
		}
		session, err := p.client.Attach(ctx, t.ID)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.targetID, p.sessionID = t.ID, session
		p.mu.Unlock()
		_, _ = p.client.Call(ctx, session, "Page.enable", nil)
		w, h, err := p.client.LayoutViewport(ctx, session)
		if err == nil {
			p.mu.Lock()
			p.cssW, p.cssH = w, h
			p.mu.Unlock()
		}
		return nil
	}
	return errNoPageTarget
}

var errNoPageTarget = errorString("浏览器没有可同步的页面标签")

type errorString string

func (e errorString) Error() string { return string(e) }

// attachEventHandlers wires CDP target-lifecycle events: tabs
// opened or closed on the master are mirrored to followers, and
// each peer keeps its own current-target bookkeeping. Handlers
// run on the CDP client's event goroutine, never inside its read
// loop, so issuing commands on the same client here is safe.
func (s *SyncService) attachEventHandlers() {
	s.mu.Lock()
	peers := s.peers
	masterID := s.masterID
	s.mu.Unlock()
	for id, peer := range peers {
		peerID := id
		peerRef := peer
		peer.client.SetEventHandler(func(ev cdp.Event) {
			s.onTargetEvent(peerID, peerRef, ev, peerID == masterID)
		})
	}
}

type targetEventParams struct {
	TargetInfo struct {
		TargetID string `json:"targetId"`
		Type     string `json:"type"`
		URL      string `json:"url"`
	} `json:"targetInfo"`
	TargetID string `json:"targetId"`
}

func (s *SyncService) onTargetEvent(peerID string, peer *syncPeer, ev cdp.Event, isMaster bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var params targetEventParams
	_ = json.Unmarshal(ev.Params, &params)
	switch ev.Method {
	case "Target.targetCreated":
		info := params.TargetInfo
		if info.Type != "page" || info.TargetID == "" {
			return
		}
		if isMaster {
			s.forEachFollower(func(f *syncPeer) {
				newID, err := f.client.CreateTarget(ctx, info.URL)
				if err != nil {
					f.markFailed("新建标签页同步失败：" + err.Error())
					return
				}
				f.mu.Lock()
				f.targetByMaster[info.TargetID] = newID
				f.mu.Unlock()
				if err := f.switchToTarget(ctx, newID); err != nil {
					f.markFailed("切换标签页同步失败：" + err.Error())
				}
			})
		}
	case "Target.targetDestroyed":
		destroyed := params.TargetID
		if destroyed == "" {
			destroyed = params.TargetInfo.TargetID
		}
		if isMaster {
			s.forEachFollower(func(f *syncPeer) {
				f.mu.Lock()
				mapped, ok := f.targetByMaster[destroyed]
				if ok {
					delete(f.targetByMaster, destroyed)
				}
				f.mu.Unlock()
				if ok {
					_ = f.client.CloseTarget(ctx, mapped)
				}
			})
		}
		peer.mu.Lock()
		wasCurrent := peer.targetID == destroyed
		peer.mu.Unlock()
		if wasCurrent {
			_ = peer.attachToCurrentPage(ctx)
		}
	case "Target.targetInfoChanged":
		if !isMaster {
			return
		}
		info := params.TargetInfo
		peer.mu.Lock()
		isCurrent := peer.targetID == info.TargetID
		peer.mu.Unlock()
		if !isCurrent || info.URL == "" {
			return
		}
		s.forEachFollower(func(f *syncPeer) {
			f.mu.Lock()
			session := f.sessionID
			f.mu.Unlock()
			if session == "" {
				return
			}
			if err := f.client.Navigate(ctx, session, info.URL); err != nil {
				f.markFailed("页面导航同步失败：" + err.Error())
			}
		})
	}
}

// switchToTarget attaches the peer to a target and makes it the
// current one.
func (p *syncPeer) switchToTarget(ctx context.Context, targetID string) error {
	session, err := p.client.Attach(ctx, targetID)
	if err != nil {
		return err
	}
	_ = p.client.ActivateTarget(ctx, targetID)
	p.mu.Lock()
	p.targetID, p.sessionID = targetID, session
	p.mu.Unlock()
	_, _ = p.client.Call(ctx, session, "Page.enable", nil)
	if w, h, err := p.client.LayoutViewport(ctx, session); err == nil {
		p.mu.Lock()
		p.cssW, p.cssH = w, h
		p.mu.Unlock()
	}
	return nil
}

func (s *SyncService) forEachFollower(fn func(*syncPeer)) {
	s.mu.Lock()
	master := s.masterID
	peers := make([]*syncPeer, 0, len(s.peers))
	for id, p := range s.peers {
		if id != master {
			peers = append(peers, p)
		}
	}
	s.mu.Unlock()
	for _, p := range peers {
		p.mu.Lock()
		failed := p.failed
		p.mu.Unlock()
		if !failed {
			fn(p)
		}
	}
}

// ---------- input event queue (hook-safe) ----------

// inputEventQueue is the bounded hand-off between the Windows
// low-level hook callback and the dispatch loop. The hook
// callback must return immediately — Windows unhooks slow
// low-level hooks and a blocked callback stalls the user's real
// input — so push NEVER blocks: when the queue is full it evicts
// the oldest disposable event. Mouse moves go first, then press
// events (a lost press leaves nothing stuck; its later release
// is a harmless no-op). Release events (key up, button up) are
// evicted only when the queue holds nothing else, because losing
// one would leave a key or button stuck on every follower.
type inputEventQueue struct {
	mu       sync.Mutex
	items    []wininput.Event
	capacity int
	notify   chan struct{}
	dropped  int
}

func newInputEventQueue(capacity int) *inputEventQueue {
	return &inputEventQueue{capacity: capacity, notify: make(chan struct{}, 1)}
}

// push enqueues one event without ever blocking.
func (q *inputEventQueue) push(ev wininput.Event) {
	q.mu.Lock()
	if len(q.items) >= q.capacity {
		idx := -1
		for i, it := range q.items {
			if it.Kind == wininput.EventMouseMove {
				idx = i
				break
			}
		}
		if idx < 0 {
			for i, it := range q.items {
				if it.Kind == wininput.EventMouseDown || it.Kind == wininput.EventKeyDown {
					idx = i
					break
				}
			}
		}
		if idx < 0 {
			idx = 0
		}
		q.items = append(q.items[:idx], q.items[idx+1:]...)
		q.dropped++
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// drain removes and returns all currently queued events, in
// order.
func (q *inputEventQueue) drain() []wininput.Event {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return nil
	}
	out := q.items
	q.items = nil
	return out
}

func (q *inputEventQueue) droppedCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.dropped
}

// ---------- pressed-input state (service-owned) ----------

// pressedState records every key and mouse button the session
// has pushed down on followers and not yet released, per channel
// (CDP page input vs native UI input). It is owned by the
// SyncService for the whole session — the dispatcher updates it,
// pause/stop release from it — so there is exactly one place
// that knows what needs letting go.
type pressedState struct {
	mu            sync.Mutex
	cdpKeys       map[int]bool
	cdpButtons    map[string]bool
	nativeKeys    map[int]bool
	nativeButtons map[string]bool
	// focusContent records whether the master's input focus was
	// most recently in page content (vs browser chrome), from
	// the last classified mouse event. Keyboard events carry no
	// coordinates, so they are routed by this memory — keyboard
	// input follows the focus the user's clicks established.
	focusContent bool
}

func newPressedState() *pressedState {
	return &pressedState{
		cdpKeys: map[int]bool{}, cdpButtons: map[string]bool{},
		nativeKeys: map[int]bool{}, nativeButtons: map[string]bool{},
		focusContent: true,
	}
}

// snapshotAndClear returns the pressed sets and resets them.
func (p *pressedState) snapshotAndClear() (cdpKeys map[int]bool, cdpButtons map[string]bool, nativeKeys map[int]bool, nativeButtons map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cdpKeys, cdpButtons, nativeKeys, nativeButtons = p.cdpKeys, p.cdpButtons, p.nativeKeys, p.nativeButtons
	p.cdpKeys = map[int]bool{}
	p.cdpButtons = map[string]bool{}
	p.nativeKeys = map[int]bool{}
	p.nativeButtons = map[string]bool{}
	return
}

func (s *SyncService) startDispatch() error {
	queue := newInputEventQueue(512)
	s.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	s.stopDisp = cancel
	pressed := s.pressed
	s.mu.Unlock()
	if err := wininput.StartCapture(func(ev wininput.Event) {
		// Hook callback: enqueue and return. Nothing here may
		// wait — not on the queue, not on CDP, not on windows.
		queue.push(ev)
	}); err != nil {
		cancel()
		return err
	}
	go s.dispatchLoop(ctx, queue, pressed)
	return nil
}

func (s *SyncService) dispatchLoop(ctx context.Context, queue *inputEventQueue, pressed *pressedState) {
	var lastPress struct {
		button string
		x, y   int32
		at     time.Time
		count  int
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-queue.notify:
		}
		for _, ev := range queue.drain() {
			// Consecutive moves are handled one by one here; the
			// queue-level eviction already shed stale moves
			// under load, so no extra coalescing is needed.
			s.handleEvent(ev, pressed, &lastPress)
		}
	}
}

// handleEvent routes one captured event: page content goes over
// CDP; browser chrome goes through native mirroring. Events that
// do not belong to the master instance are dropped.
func (s *SyncService) handleEvent(ev wininput.Event, pressed *pressedState, lastPress *struct {
	button string
	x, y   int32
	at     time.Time
	count  int
},
) {
	s.mu.Lock()
	state := s.state
	masterID := s.masterID
	master := s.peers[masterID]
	s.mu.Unlock()
	if state != SyncStateActive || master == nil {
		return
	}
	masterPID, ok := s.instances.instanceProcessID(masterID)
	if !ok {
		return
	}

	if ev.Kind == wininput.EventKeyDown || ev.Kind == wininput.EventKeyUp {
		fg := wininput.ForegroundWindow()
		if fg == 0 || wininput.WindowPID(fg) != uint32(masterPID) {
			return
		}
		if ev.Kind == wininput.EventKeyUp {
			noteKeyUp(ev.VK)
		}
		pressed.mu.Lock()
		inContent := pressed.focusContent
		pressed.mu.Unlock()
		if inContent {
			if h, ok := contentHitOfMaster(uint32(masterPID)); ok {
				s.dispatchPageEvent(ev, master, h, pressed, lastPress)
				return
			}
		}
		s.dispatchNativeEvent(ev, masterID, uint32(masterPID), wininput.Hit{Window: fg}, pressed)
		return
	}
	hit, ok := wininput.HitTest(ev.X, ev.Y)
	if !ok || hit.PID != uint32(masterPID) {
		return
	}
	pressed.mu.Lock()
	pressed.focusContent = hit.IsContent
	pressed.mu.Unlock()
	if hit.IsContent && !hit.ContentRect.Empty() {
		s.dispatchPageEvent(ev, master, hit, pressed, lastPress)
		return
	}
	s.dispatchNativeEvent(ev, masterID, uint32(masterPID), hit, pressed)
}

// contentHitOfMaster finds the master's content rectangle by
// hit-testing the center of its main window.
func contentHitOfMaster(masterPID uint32) (wininput.Hit, bool) {
	hwnd, ok := wininput.TopWindowForPID(masterPID)
	if !ok {
		return wininput.Hit{}, false
	}
	r, ok := wininput.GetWindowRect(hwnd)
	if !ok {
		return wininput.Hit{}, false
	}
	h, ok := wininput.HitTest(r.X+r.Width/2, r.Y+r.Height/2)
	if !ok || !h.IsContent {
		return wininput.Hit{}, false
	}
	return h, true
}

// dispatchPageEvent mirrors one page-content event to every
// follower over CDP, mapping coordinates through the content
// rectangles and CSS viewports.
func (s *SyncService) dispatchPageEvent(ev wininput.Event, master *syncPeer, hit wininput.Hit, pressed *pressedState, lastPress *struct {
	button string
	x, y   int32
	at     time.Time
	count  int
},
) {
	master.mu.Lock()
	mCssW, mCssH := master.cssW, master.cssH
	master.mu.Unlock()
	if mCssW <= 0 || mCssH <= 0 {
		return
	}
	cssX := float64(ev.X-hit.ContentRect.X) * mCssW / float64(hit.ContentRect.Width)
	cssY := float64(ev.Y-hit.ContentRect.Y) * mCssH / float64(hit.ContentRect.Height)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.forEachFollower(func(f *syncPeer) {
		f.mu.Lock()
		session, fCssW, fCssH := f.sessionID, f.cssW, f.cssH
		f.mu.Unlock()
		if session == "" || fCssW <= 0 {
			return
		}
		x := cssX * fCssW / mCssW
		y := cssY * fCssH / mCssH
		var err error
		switch ev.Kind {
		case wininput.EventMouseMove:
			err = f.client.MouseEvent(ctx, session, "mouseMoved", x, y, "none", ev.ButtonsMask, 0, 0, 0)
		case wininput.EventMouseDown:
			count := 1
			if lastPress.button == ev.Button && time.Since(lastPress.at) < 500*time.Millisecond &&
				abs32(ev.X-lastPress.x) < 5 && abs32(ev.Y-lastPress.y) < 5 {
				count = lastPress.count + 1
			}
			lastPress.button, lastPress.x, lastPress.y, lastPress.at, lastPress.count = ev.Button, ev.X, ev.Y, time.Now(), count
			pressed.mu.Lock()
			pressed.cdpButtons[ev.Button] = true
			pressed.mu.Unlock()
			err = f.client.MouseEvent(ctx, session, "mousePressed", x, y, ev.Button, ev.ButtonsMask, count, 0, 0)
		case wininput.EventMouseUp:
			pressed.mu.Lock()
			delete(pressed.cdpButtons, ev.Button)
			pressed.mu.Unlock()
			err = f.client.MouseEvent(ctx, session, "mouseReleased", x, y, ev.Button, ev.ButtonsMask, 1, 0, 0)
		case wininput.EventMouseWheel:
			err = f.client.MouseEvent(ctx, session, "mouseWheel", x, y, "none", ev.ButtonsMask, 0, 0, -float64(ev.WheelDelta))
		case wininput.EventKeyDown, wininput.EventKeyUp:
			typ := "keyDown"
			if ev.Kind == wininput.EventKeyUp {
				typ = "keyUp"
			}
			text, key := keyText(ev.VK)
			mods := currentModifiers()
			if ev.Kind == wininput.EventKeyDown {
				pressed.mu.Lock()
				pressed.cdpKeys[ev.VK] = true
				pressed.mu.Unlock()
				err = f.client.KeyEvent(ctx, session, typ, ev.VK, mods, text, key)
			} else {
				pressed.mu.Lock()
				delete(pressed.cdpKeys, ev.VK)
				pressed.mu.Unlock()
				err = f.client.KeyEvent(ctx, session, typ, ev.VK, mods, "", key)
			}
		}
		if err != nil {
			f.markFailed("页面操作同步失败：" + err.Error())
		}
	})
}

// matchFollowerWindow decides which of a follower's windows
// receives a native event that hit masterHit on the master
// instance, and which rectangle to map coordinates through.
//
//   - Master main window → follower main window.
//   - A popup (any other top-level window of the master
//     process): the follower must have EXACTLY ONE visible
//     candidate with the same window class and the same
//     ownership shape (owned by its main window, or unowned,
//     matching the master's popup). Coordinates then map
//     through the popup rectangles.
//   - Anything else — no candidate, several candidates, class
//     or ownership mismatch — is NOT delivered: guessing would
//     send input to a window the user never touched.
func matchFollowerWindow(masterHit, masterMain wininput.WindowInfo, followerWindows []wininput.WindowInfo, followerMain wininput.WindowInfo) (target wininput.WindowInfo, mapFrom wininput.Rect, ok bool) {
	if masterHit.HWND == masterMain.HWND {
		return followerMain, masterMain.Rect, true
	}
	// Popup case: structural correspondence only.
	masterOwned := masterHit.Owner == masterMain.HWND
	masterUnowned := masterHit.Owner == 0
	if !masterOwned && !masterUnowned {
		return wininput.WindowInfo{}, wininput.Rect{}, false
	}
	var candidates []wininput.WindowInfo
	for _, w := range followerWindows {
		if w.HWND == followerMain.HWND || !w.Visible {
			continue
		}
		if w.Class != masterHit.Class {
			continue
		}
		if masterOwned && w.Owner != followerMain.HWND {
			continue
		}
		if masterUnowned && w.Owner != 0 {
			continue
		}
		candidates = append(candidates, w)
	}
	if len(candidates) != 1 {
		return wininput.WindowInfo{}, wininput.Rect{}, false
	}
	return candidates[0], masterHit.Rect, true
}

// dispatchNativeEvent mirrors one browser-chrome event to every
// follower with native input: resolve the follower window that
// corresponds to the window actually hit on the master (main
// window or a structurally matched popup), activate it, inject
// at the relatively mapped point, then return focus to the
// master's window. Followers without a confirmed counterpart
// are skipped for this event — input is never guessed onto
// their main window.
func (s *SyncService) dispatchNativeEvent(ev wininput.Event, masterID string, masterPID uint32, hit wininput.Hit, pressed *pressedState) {
	masterMainHWND, ok := wininput.TopWindowForPID(masterPID)
	if !ok {
		return
	}
	masterMainRect, _ := wininput.GetWindowRect(masterMainHWND)
	masterHit := wininput.WindowInfo{
		HWND: hit.Window, Class: wininput.WindowClass(hit.Window),
		Owner: wininput.OwnerWindow(hit.Window), Rect: hit.WindowRect,
		Visible: true,
	}
	masterMain := wininput.WindowInfo{HWND: masterMainHWND, Rect: masterMainRect}
	isPopup := hit.Window != 0 && hit.Window != masterMainHWND

	s.mu.Lock()
	ids := make([]string, 0, len(s.peers))
	for id := range s.peers {
		if id != masterID {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	for _, id := range ids {
		pid, ok := s.instances.instanceProcessID(id)
		if !ok {
			continue
		}
		followerMainHWND, ok := wininput.TopWindowForPID(uint32(pid))
		if !ok {
			continue
		}
		followerMainRect, _ := wininput.GetWindowRect(followerMainHWND)
		followerMain := wininput.WindowInfo{HWND: followerMainHWND, Rect: followerMainRect}

		target := followerMain
		mapFrom := masterMainRect
		if isPopup {
			matched, from, ok := matchFollowerWindow(masterHit, masterMain,
				wininput.ListWindowsForPID(uint32(pid)), followerMain)
			if !ok {
				continue // no confirmed counterpart: skip, never guess
			}
			target, mapFrom = matched, from
		}
		mapped := ev
		mapped.X, mapped.Y = wininput.MapPoint(ev.X, ev.Y, mapFrom, target.Rect)
		wininput.ActivateWindow(target.HWND)
		switch ev.Kind {
		case wininput.EventKeyDown:
			pressed.mu.Lock()
			pressed.nativeKeys[ev.VK] = true
			pressed.mu.Unlock()
			wininput.InjectKey(ev.VK, true)
		case wininput.EventKeyUp:
			pressed.mu.Lock()
			delete(pressed.nativeKeys, ev.VK)
			pressed.mu.Unlock()
			wininput.InjectKey(ev.VK, false)
		default:
			if ev.Kind == wininput.EventMouseDown {
				pressed.mu.Lock()
				pressed.nativeButtons[ev.Button] = true
				pressed.mu.Unlock()
			}
			if ev.Kind == wininput.EventMouseUp {
				pressed.mu.Lock()
				delete(pressed.nativeButtons, ev.Button)
				pressed.mu.Unlock()
			}
			wininput.InjectMouse(mapped)
		}
	}
	wininput.ActivateWindow(masterMainHWND)
}

// releaseFollowerInputs lets go of EVERY key and mouse button
// the session currently holds down on followers, on both
// channels (pause/stop/failure paths), so nothing can stay
// stuck. Only state this session injected is released — the
// master's real keyboard is never touched: CDP releases go to
// follower sessions only, and native releases are injected
// while each follower window is briefly foreground, after which
// the master's window is re-activated.
func (s *SyncService) releaseFollowerInputs() {
	s.mu.Lock()
	pressed := s.pressed
	masterID := s.masterID
	s.mu.Unlock()
	if pressed == nil {
		return
	}
	cdpKeys, cdpButtons, nativeKeys, nativeButtons := pressed.snapshotAndClear()

	if len(cdpKeys) > 0 || len(cdpButtons) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.forEachFollower(func(f *syncPeer) {
			f.mu.Lock()
			session := f.sessionID
			f.mu.Unlock()
			if session == "" {
				return
			}
			for vk := range cdpKeys {
				_, key := keyText(vk)
				noteKeyUp(vk)
				_ = f.client.KeyEvent(ctx, session, "keyUp", vk, 0, "", key)
			}
			for button := range cdpButtons {
				_ = f.client.MouseEvent(ctx, session, "mouseReleased", 0, 0, button, 0, 1, 0, 0)
			}
		})
		cancel()
	}

	if len(nativeKeys) > 0 || len(nativeButtons) > 0 {
		s.mu.Lock()
		ids := make([]string, 0, len(s.peers))
		for id := range s.peers {
			if id != masterID {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()
		var masterHWND uintptr
		if pid, ok := s.instances.instanceProcessID(masterID); ok {
			masterHWND, _ = wininput.TopWindowForPID(uint32(pid))
		}
		for _, id := range ids {
			pid, ok := s.instances.instanceProcessID(id)
			if !ok {
				continue
			}
			hwnd, ok := wininput.TopWindowForPID(uint32(pid))
			if !ok {
				continue
			}
			rect, _ := wininput.GetWindowRect(hwnd)
			wininput.ActivateWindow(hwnd)
			for vk := range nativeKeys {
				noteKeyUp(vk)
				wininput.InjectKey(vk, false)
			}
			for button := range nativeButtons {
				wininput.InjectMouse(wininput.Event{
					Kind: wininput.EventMouseUp, Button: button,
					X: rect.X + rect.Width/2, Y: rect.Y + rect.Height/2,
				})
			}
		}
		if masterHWND != 0 {
			wininput.ActivateWindow(masterHWND)
		}
	}
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// currentModifiers reads the dispatcher's tracked modifier state
// (Alt=1, Ctrl=2, Meta=4, Shift=8 for CDP).
var modifierState struct {
	sync.Mutex
	shift, ctrl, alt, meta bool
}

func currentModifiers() int {
	modifierState.Lock()
	defer modifierState.Unlock()
	mods := 0
	if modifierState.alt {
		mods |= 1
	}
	if modifierState.ctrl {
		mods |= 2
	}
	if modifierState.meta {
		mods |= 4
	}
	if modifierState.shift {
		mods |= 8
	}
	return mods
}

// keyText maps a virtual-key code to the text and key name a CDP
// key event should carry for direct (non-IME) input on a standard
// layout. Modifier keys update the shared modifier state as a
// side effect of the dispatch calling order. IME-composed text
// (e.g. Chinese input methods) does not arrive as virtual-key
// text and is NOT reproduced by this mapping — see the Stage 10
// notes in the PR description.
func keyText(vk int) (text, key string) {
	switch vk {
	case 0x10, 0xA0, 0xA1: // Shift
		modifierState.Lock()
		modifierState.shift = true
		modifierState.Unlock()
		return "", "Shift"
	case 0x11, 0xA2, 0xA3: // Ctrl
		modifierState.Lock()
		modifierState.ctrl = true
		modifierState.Unlock()
		return "", "Control"
	case 0x12, 0xA4, 0xA5: // Alt
		modifierState.Lock()
		modifierState.alt = true
		modifierState.Unlock()
		return "", "Alt"
	case 0x5B, 0x5C: // Win
		modifierState.Lock()
		modifierState.meta = true
		modifierState.Unlock()
		return "", "Meta"
	case 0x0D:
		return "\r", "Enter"
	case 0x09:
		return "\t", "Tab"
	case 0x20:
		return " ", " "
	case 0x08:
		return "", "Backspace"
	case 0x1B:
		return "", "Escape"
	}
	if vk >= 'A' && vk <= 'Z' {
		ch := byte(vk)
		modifierState.Lock()
		shift := modifierState.shift
		modifierState.Unlock()
		if !shift {
			ch += 'a' - 'A'
		}
		return string([]byte{ch}), string([]byte{byte(vk)})
	}
	if vk >= '0' && vk <= '9' {
		return string([]byte{byte(vk)}), string([]byte{byte(vk)})
	}
	return "", ""
}

// noteKeyUp clears modifier state on key release.
func noteKeyUp(vk int) {
	modifierState.Lock()
	defer modifierState.Unlock()
	switch vk {
	case 0x10, 0xA0, 0xA1:
		modifierState.shift = false
	case 0x11, 0xA2, 0xA3:
		modifierState.ctrl = false
	case 0x12, 0xA4, 0xA5:
		modifierState.alt = false
	case 0x5B, 0x5C:
		modifierState.meta = false
	}
}
