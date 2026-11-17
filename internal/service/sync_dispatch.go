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
// each peer keeps its own current-target bookkeeping.
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
			// A tab opened on the master: open the same URL on
			// every follower and make it their current tab.
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
			// The master's current tab may be gone; re-attach to
			// whatever page remains.
			peer.mu.Lock()
			wasCurrent := peer.targetID == destroyed
			peer.mu.Unlock()
			if wasCurrent {
				_ = peer.attachToCurrentPage(ctx)
			}
		} else {
			peer.mu.Lock()
			wasCurrent := peer.targetID == destroyed
			peer.mu.Unlock()
			if wasCurrent {
				_ = peer.attachToCurrentPage(ctx)
			}
		}
	case "Target.targetInfoChanged":
		// Address-bar navigations (and history traversals) on the
		// master surface here as URL changes of its current tab:
		// navigate the followers' corresponding tabs.
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

// ---------- input capture dispatch ----------

// pressedState tracks what the session has pushed down on the
// followers, so pause/stop can release every key and button.
type pressedState struct {
	mu      sync.Mutex
	keys    map[int]bool
	buttons map[string]bool
	// focusContent records whether the master's input focus was
	// most recently in page content (vs browser chrome), from
	// the last classified mouse event. Keyboard events carry no
	// coordinates, so they are routed by this memory — keyboard
	// input follows the focus the user's clicks established.
	focusContent bool
}

func (s *SyncService) startDispatch() error {
	events := make(chan wininput.Event, 256)
	s.mu.Lock()
	s.events = events
	ctx, cancel := context.WithCancel(context.Background())
	s.stopDisp = cancel
	s.mu.Unlock()
	if err := wininput.StartCapture(func(ev wininput.Event) {
		select {
		case events <- ev:
		default:
			// A full queue drops the OLDEST kind of traffic that
			// is safe to lose: moves. Everything else blocks
			// briefly rather than breaking press/release pairs.
			if ev.Kind == wininput.EventMouseMove {
				return
			}
			events <- ev
		}
	}); err != nil {
		cancel()
		return err
	}
	go s.dispatchLoop(ctx, events, &pressedState{
		keys: map[int]bool{}, buttons: map[string]bool{}, focusContent: true,
	})
	return nil
}

func (s *SyncService) dispatchLoop(ctx context.Context, events chan wininput.Event, pressed *pressedState) {
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
		case ev := <-events:
			// Coalesce queued mouse moves: only the newest
			// position matters.
			if ev.Kind == wininput.EventMouseMove {
				drained := true
				for drained {
					select {
					case newer := <-events:
						if newer.Kind == wininput.EventMouseMove {
							ev = newer
						} else {
							s.handleEvent(newer, pressed, &lastPress)
						}
					default:
						drained = false
					}
				}
			}
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

	// Resolve the event's source window and content-ness.
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
			// Page keys need the master's CURRENT content rect
			// for wheel-free coordinate-free dispatch; keyboard
			// events ignore coordinates, so an empty hit with
			// the content flag is enough — but dispatchPageEvent
			// maps mouse coordinates only, and for keys it uses
			// the hit merely for classification. Fetch the real
			// content rect from the master's window center.
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
	// Master CSS-pixel coordinates of the event.
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
			pressed.buttons[ev.Button] = true
			pressed.mu.Unlock()
			err = f.client.MouseEvent(ctx, session, "mousePressed", x, y, ev.Button, ev.ButtonsMask, count, 0, 0)
		case wininput.EventMouseUp:
			pressed.mu.Lock()
			delete(pressed.buttons, ev.Button)
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
				pressed.keys[ev.VK] = true
				pressed.mu.Unlock()
				err = f.client.KeyEvent(ctx, session, typ, ev.VK, mods, text, key)
			} else {
				pressed.mu.Lock()
				delete(pressed.keys, ev.VK)
				pressed.mu.Unlock()
				err = f.client.KeyEvent(ctx, session, typ, ev.VK, mods, "", key)
			}
		}
		if err != nil {
			f.markFailed("页面操作同步失败：" + err.Error())
		}
	})
}

// dispatchNativeEvent mirrors one browser-chrome event to every
// follower with native input: activate the follower's
// corresponding window, inject at the relatively mapped point,
// then return focus to the master's window.
func (s *SyncService) dispatchNativeEvent(ev wininput.Event, masterID string, masterPID uint32, hit wininput.Hit, pressed *pressedState) {
	masterHWND, ok := wininput.TopWindowForPID(masterPID)
	if !ok {
		return
	}
	// The window the user actually touched may be a popup owned
	// by the master's main window; followers use their own
	// corresponding popup when one exists, else their main
	// window.
	masterRect := hit.WindowRect
	if masterRect.Empty() {
		if r, ok := wininput.GetWindowRect(hit.Window); ok {
			masterRect = r
		}
	}
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
		hwnd, ok := wininput.TopWindowForPID(uint32(pid))
		if !ok {
			continue
		}
		rect, ok := wininput.GetWindowRect(hwnd)
		if !ok {
			continue
		}
		mapped := ev
		mapped.X, mapped.Y = wininput.MapPoint(ev.X, ev.Y, masterRect, rect)
		wininput.ActivateWindow(hwnd)
		switch ev.Kind {
		case wininput.EventKeyDown:
			pressed.mu.Lock()
			pressed.keys[ev.VK] = true
			pressed.mu.Unlock()
			wininput.InjectKey(ev.VK, true)
		case wininput.EventKeyUp:
			pressed.mu.Lock()
			delete(pressed.keys, ev.VK)
			pressed.mu.Unlock()
			wininput.InjectKey(ev.VK, false)
		default:
			if ev.Kind == wininput.EventMouseDown {
				pressed.mu.Lock()
				pressed.buttons[ev.Button] = true
				pressed.mu.Unlock()
			}
			if ev.Kind == wininput.EventMouseUp {
				pressed.mu.Lock()
				delete(pressed.buttons, ev.Button)
				pressed.mu.Unlock()
			}
			wininput.InjectMouse(mapped)
		}
	}
	wininput.ActivateWindow(masterHWND)
}

// releaseFollowerInputs lets go of every key and mouse button the
// session currently holds down on followers (pause/stop/failure
// paths), so nothing can stay stuck.
func (s *SyncService) releaseFollowerInputs() {
	// Native side: release the common modifier and button state
	// by re-issuing ups for the tracked set kept by the
	// dispatcher. The dispatcher owns the pressed set; this
	// service-level sweep covers the CDP side per peer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.forEachFollower(func(f *syncPeer) {
		f.mu.Lock()
		session := f.sessionID
		f.mu.Unlock()
		if session == "" {
			return
		}
		_ = f.client.MouseEvent(ctx, session, "mouseReleased", 0, 0, "left", 0, 1, 0, 0)
		_ = f.client.MouseEvent(ctx, session, "mouseReleased", 0, 0, "right", 0, 1, 0, 0)
		_ = f.client.MouseEvent(ctx, session, "mouseReleased", 0, 0, "middle", 0, 1, 0, 0)
	})
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// currentModifiers reads the dispatcher's tracked modifier state
// from the last key events. Kept simple: the CDP modifiers
// bitmask (Alt=1, Ctrl=2, Meta=4, Shift=8) is derived from the
// pressed-key set the hook stream maintains globally.
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

// noteKeyUp clears modifier state on key release; called from the
// dispatch path for key-up events before keyText is consulted.
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
