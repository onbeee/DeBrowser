package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// Stage 10 v3 follower worker. Each follower instance owns: one
// CDP connection, one page tracker, one bounded action queue and
// one dispatch goroutine — the only sender of input commands on
// that connection. A slow or failing follower never blocks the
// master intake or any other follower: its queue is its own, its
// failures are recorded on itself, and critical events are never
// silently dropped — if a critical event cannot be queued, the
// follower is marked failed and its pressed state is released.

const followerQueueCapacity = 256

type syncActionKind int

const (
	actionInput syncActionKind = iota
	actionNavigate
	actionReload
	actionActivate
	actionCreate
	actionClose
	actionRelease
)

type syncAction struct {
	kind   syncActionKind
	ev     PageEvent // actionInput: the master's page event
	target string    // follower target the action applies to
	url    string    // actionNavigate / actionCreate

	// actionCreate completes through reply (buffered, size 1).
	reply chan createTargetResult
}

type createTargetResult struct {
	targetID string
	err      error
}

// Follower states reported to the UI.
const (
	followerReady       = "ready"
	followerUnconfirmed = "unconfirmed"
	followerFailed      = "failed"
)

type followerWorker struct {
	instanceID string
	client     *cdp.Client // nil when the connection never came up
	tracker    *pageTracker

	notify chan struct{}
	done   chan struct{}

	mu             sync.Mutex
	queue          []syncAction
	state          string
	reason         string
	targetMap      map[string]string // master targetID -> follower targetID
	activeTarget   string            // follower target currently following the master's active page
	viewportW      float64
	viewportH      float64
	masterW        float64
	masterH        float64
	pressedButtons int
	pressedKeys    map[string]PageEvent // by code
	lastX, lastY   float64
	stopped        bool
}

func newFollowerWorker(instanceID string) *followerWorker {
	return &followerWorker{
		instanceID:  instanceID,
		notify:      make(chan struct{}, 1),
		done:        make(chan struct{}),
		state:       followerUnconfirmed,
		targetMap:   make(map[string]string),
		pressedKeys: make(map[string]PageEvent),
	}
}

// markFailed records a terminal failure for this follower only.
func (w *followerWorker) markFailed(reason string) {
	w.mu.Lock()
	if w.state != followerFailed {
		w.state = followerFailed
		w.reason = reason
	}
	w.queue = nil
	w.mu.Unlock()
	w.signal()
}

// setUnconfirmed records that this follower's active page cannot
// currently be determined; it receives no input meanwhile.
func (w *followerWorker) setUnconfirmed(reason string) {
	w.mu.Lock()
	if w.state != followerFailed {
		w.state = followerUnconfirmed
		w.reason = reason
	}
	w.mu.Unlock()
}

func (w *followerWorker) setReady() {
	w.mu.Lock()
	if w.state != followerFailed {
		w.state = followerReady
		w.reason = ""
	}
	w.mu.Unlock()
}

// mapTarget records an explicit master->follower target mapping.
func (w *followerWorker) mapTarget(masterTarget, followerTarget string) {
	w.mu.Lock()
	w.targetMap[masterTarget] = followerTarget
	w.mu.Unlock()
}

func (w *followerWorker) unmapTarget(masterTarget string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ft, ok := w.targetMap[masterTarget]
	if ok {
		delete(w.targetMap, masterTarget)
		if w.activeTarget == ft {
			w.activeTarget = ""
		}
	}
	return ft, ok
}

func (w *followerWorker) mappedTarget(masterTarget string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ft, ok := w.targetMap[masterTarget]
	return ft, ok
}

func (w *followerWorker) setActiveTarget(followerTarget string) {
	w.mu.Lock()
	w.activeTarget = followerTarget
	w.mu.Unlock()
}

func (w *followerWorker) signal() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// enqueueInput queues one master page event for dispatch to the
// given follower target. Pointer moves coalesce at the queue tail;
// critical events are ordered and never dropped silently.
func (w *followerWorker) enqueueInput(ev PageEvent, followerTarget string) {
	w.mu.Lock()
	if w.state != followerReady || w.stopped {
		w.mu.Unlock()
		return
	}
	act := syncAction{kind: actionInput, ev: ev, target: followerTarget}
	if ev.Kind == "pointermove" {
		if n := len(w.queue); n > 0 {
			if last := w.queue[n-1]; last.kind == actionInput && last.ev.Kind == "pointermove" && last.target == followerTarget {
				w.queue[n-1] = act
				w.mu.Unlock()
				return
			}
		}
		if len(w.queue) >= followerQueueCapacity {
			w.mu.Unlock()
			return // moves are expendable under overload; critical events are not
		}
	} else if len(w.queue) >= followerQueueCapacity {
		w.mu.Unlock()
		w.markFailed("跟随实例事件队列已满，无法保证输入完整")
		w.releaseAsync()
		return
	}
	w.queue = append(w.queue, act)
	w.mu.Unlock()
	w.signal()
}

// enqueueControl queues a structural action (navigate, reload,
// activate, close, release). It reports whether the action was
// accepted.
func (w *followerWorker) enqueueControl(act syncAction) bool {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return false
	}
	if len(w.queue) >= followerQueueCapacity {
		w.mu.Unlock()
		return false
	}
	w.queue = append(w.queue, act)
	w.mu.Unlock()
	w.signal()
	return true
}

// createTarget asks the dispatch loop to create a follower page
// and waits for the created target ID, so the mapping is built
// from the actual creation result — never from titles, URLs or
// list order.
func (w *followerWorker) createTarget(ctx context.Context, url string) (string, error) {
	reply := make(chan createTargetResult, 1)
	if !w.enqueueControl(syncAction{kind: actionCreate, url: url, reply: reply}) {
		return "", fmt.Errorf("跟随实例忙，无法创建页面")
	}
	select {
	case res := <-reply:
		return res.targetID, res.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-w.done:
		return "", fmt.Errorf("跟随实例已停止")
	}
}

// clearPending drops every queued action (pause semantics:
// nothing stale is replayed on resume).
func (w *followerWorker) clearPending() {
	w.mu.Lock()
	w.queue = nil
	w.mu.Unlock()
}

// releaseAsync queues an input-state release behind pending work.
func (w *followerWorker) releaseAsync() {
	w.enqueueControl(syncAction{kind: actionRelease})
}

// stop ends the dispatch loop after a final release attempt. It is
// used for every follower, including failed ones.
func (w *followerWorker) stop() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.mu.Unlock()
	if w.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		w.releaseAll(ctx)
		cancel()
	}
	close(w.done)
	w.signal()
}

// run is the dispatch loop: the single sender of commands on this
// follower's connection.
func (w *followerWorker) run() {
	for {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		if len(w.queue) == 0 {
			w.mu.Unlock()
			select {
			case <-w.notify:
				continue
			case <-w.done:
				return
			}
		}
		act := w.queue[0]
		w.queue = w.queue[1:]
		w.mu.Unlock()
		w.execute(act)
	}
}

func (w *followerWorker) execute(act syncAction) {
	if w.client == nil {
		if act.kind == actionCreate && act.reply != nil {
			act.reply <- createTargetResult{err: fmt.Errorf("跟随实例未连接")}
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var err error
	switch act.kind {
	case actionInput:
		err = w.dispatchInput(ctx, act)
	case actionNavigate:
		err = w.pageCall(ctx, act.target, "Page.navigate", map[string]any{"url": act.url})
	case actionReload:
		err = w.pageCall(ctx, act.target, "Page.reload", nil)
	case actionActivate:
		err = w.client.ActivateTarget(ctx, act.target)
	case actionCreate:
		var id string
		id, err = w.client.CreateTarget(ctx, act.url, true)
		if act.reply != nil {
			act.reply <- createTargetResult{targetID: id, err: err}
		}
		return // creation failure is the caller's to record, not a dispatch failure
	case actionClose:
		err = w.client.CloseTarget(ctx, act.target)
	case actionRelease:
		w.releaseAll(ctx)
		return
	}
	if err != nil {
		w.markFailed("跟随实例执行失败：" + err.Error())
		w.releaseAsync()
	}
}

func (w *followerWorker) pageCall(ctx context.Context, targetID, method string, params any) error {
	sessionID, ok := w.tracker.SessionFor(targetID)
	if !ok {
		return fmt.Errorf("跟随页面未接入")
	}
	_, err := w.client.SessionCall(ctx, sessionID, method, params)
	return err
}

// dispatchInput translates one master page event into CDP Input
// commands on the follower's mapped page. Coordinates are CSS
// viewport pixels, scaled by the ratio between the follower's and
// the master's viewport when they differ.
func (w *followerWorker) dispatchInput(ctx context.Context, act syncAction) error {
	sessionID, ok := w.tracker.SessionFor(act.target)
	if !ok {
		return fmt.Errorf("跟随页面未接入")
	}
	ev := act.ev
	x, y := ev.X, ev.Y
	w.mu.Lock()
	if w.masterW > 0 && w.viewportW > 0 && w.masterW != w.viewportW {
		x = ev.X * w.viewportW / w.masterW
	}
	if w.masterH > 0 && w.viewportH > 0 && w.masterH != w.viewportH {
		y = ev.Y * w.viewportH / w.masterH
	}
	w.lastX, w.lastY = x, y
	w.mu.Unlock()

	call := func(method string, params any) error {
		_, err := w.client.SessionCall(ctx, sessionID, method, params)
		return err
	}
	switch ev.Kind {
	case "pointermove":
		return call("Input.dispatchMouseEvent", map[string]any{
			"type": "mouseMoved", "x": x, "y": y, "buttons": ev.Buttons,
		})
	case "pointerdown":
		button := mouseButtonName(ev.Button)
		if button == "" {
			return nil // an unmapped button is reported unsupported by being ignored, never guessed
		}
		w.mu.Lock()
		w.pressedButtons |= domButtonBit(ev.Button)
		w.mu.Unlock()
		return call("Input.dispatchMouseEvent", map[string]any{
			"type": "mousePressed", "x": x, "y": y,
			"button": button, "buttons": ev.Buttons, "clickCount": maxInt(ev.Detail, 1),
		})
	case "pointerup":
		button := mouseButtonName(ev.Button)
		if button == "" {
			return nil
		}
		w.mu.Lock()
		w.pressedButtons &^= domButtonBit(ev.Button)
		w.mu.Unlock()
		return call("Input.dispatchMouseEvent", map[string]any{
			"type": "mouseReleased", "x": x, "y": y,
			"button": button, "buttons": ev.Buttons, "clickCount": maxInt(ev.Detail, 1),
		})
	case "wheel":
		return call("Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y,
			"deltaX": ev.DeltaX, "deltaY": ev.DeltaY,
		})
	case "keydown", "keyup":
		if ev.IsPasteShortcut() {
			// Paste follows the insertFromPaste text path only;
			// the native shortcut is never also dispatched (that
			// would paste twice).
			return nil
		}
		// Every key event is dispatched, including plain character
		// keys that games and canvas apps listen for. The CDP
		// parameters deliberately carry NO text/unmodifiedText
		// field, so the key event itself inserts no character on
		// the follower — committed text arrives separately as a
		// text event and is inserted exactly once via
		// Input.insertText.
		w.mu.Lock()
		if ev.Kind == "keydown" {
			w.pressedKeys[ev.Code] = ev
		} else {
			delete(w.pressedKeys, ev.Code)
		}
		w.mu.Unlock()
		eventType := "keyDown"
		if ev.Kind == "keyup" {
			eventType = "keyUp"
		}
		return call("Input.dispatchKeyEvent", map[string]any{
			"type": eventType, "key": ev.Key, "code": ev.Code,
			"location": ev.Location, "modifiers": ev.cdpModifiers(),
			"autoRepeat": ev.Repeat,
		})
	case "text":
		if ev.Text == "" {
			return nil
		}
		_, err := w.client.SessionCall(ctx, sessionID, "Input.insertText",
			map[string]any{"text": ev.Text})
		return err
	}
	return nil
}

// releaseAll lifts every mouse button and key this worker pressed
// on its active page. It is best-effort cleanup: failures are
// ignored because the connection is usually already gone.
func (w *followerWorker) releaseAll(ctx context.Context) {
	w.mu.Lock()
	buttons := w.pressedButtons
	keys := make([]PageEvent, 0, len(w.pressedKeys))
	for _, ev := range w.pressedKeys {
		keys = append(keys, ev)
	}
	target := w.activeTarget
	x, y := w.lastX, w.lastY
	w.pressedButtons = 0
	w.pressedKeys = make(map[string]PageEvent)
	w.mu.Unlock()
	if w.client == nil || w.tracker == nil || target == "" {
		return
	}
	sessionID, ok := w.tracker.SessionFor(target)
	if !ok {
		return
	}
	for bit := 1; bit <= 16; bit <<= 1 {
		if buttons&bit == 0 {
			continue
		}
		_, _ = w.client.SessionCall(ctx, sessionID, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseReleased", "x": x, "y": y,
			"button": buttonBitName(bit), "buttons": 0, "clickCount": 1,
		})
	}
	for _, ev := range keys {
		_, _ = w.client.SessionCall(ctx, sessionID, "Input.dispatchKeyEvent", map[string]any{
			"type": "keyUp", "key": ev.Key, "code": ev.Code,
			"location": ev.Location, "modifiers": 0,
		})
	}
}

// snapshot returns the follower's reported state.
func (w *followerWorker) snapshot() (state, reason string, pageReady bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state, w.reason, w.state == followerReady && w.activeTarget != ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mouseButtonName maps a DOM pointer button index to the CDP
// button name. Unknown buttons return "" and are never guessed.
func mouseButtonName(button int) string {
	switch button {
	case 0:
		return "left"
	case 1:
		return "middle"
	case 2:
		return "right"
	case 3:
		return "back"
	case 4:
		return "forward"
	}
	return ""
}

// domButtonBit maps a DOM pointer button index to its bit in the
// buttons bitmask (identical in DOM and CDP).
func domButtonBit(button int) int {
	switch button {
	case 0:
		return 1
	case 1:
		return 4
	case 2:
		return 2
	case 3:
		return 8
	case 4:
		return 16
	}
	return 0
}

func buttonBitName(bit int) string {
	switch bit {
	case 1:
		return "left"
	case 2:
		return "right"
	case 4:
		return "middle"
	case 8:
		return "back"
	case 16:
		return "forward"
	}
	return "left"
}
