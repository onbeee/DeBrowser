package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/uia"
)

// Stage 10 v2 follower dispatch.
//
// The PR #17 design fanned events out through ONE shared
// dispatch loop that synchronously waited on every follower's
// CDP calls: one slow follower stalled all the others and backed
// events up into the input hook. This design gives EVERY
// follower its own bounded queue and its own dispatch goroutine
// that is the sole user of that follower's CDP connection:
//
//   - a slow or dead follower can never block another follower;
//   - mouse moves coalesce ONLY within the trailing run of
//     consecutive moves — never across a button, wheel or key
//     boundary, so event order is never rewritten;
//   - critical events (presses, releases, keys, navigation,
//     tabs) are never silently dropped: if one cannot be queued
//     safely, the follower is failed and its injected input
//     state is released, instead of drifting out of sync;
//   - pause/stop inject a release that the worker performs even
//     when the follower has already failed.
//
// No goroutine is spawned per event; each follower owns exactly
// one worker for the session's lifetime.

type syncEventKind int

const (
	evMouseMove syncEventKind = iota
	evMouseButton
	evMouseWheel
	evKey
	evNavigate
	evReload
	evHistoryDelta
	evCreateTab
	evCloseTab
	evActivateTab
	evRefreshViewport
	evUIOp
	evRelease
	evStop
	evInit
)

type syncEvent struct {
	kind syncEventKind

	// Mouse, in the MASTER page's CSS viewport coordinates.
	x, y       float64
	button     string
	buttons    int
	down       bool
	clickCount int
	deltaX     float64
	deltaY     float64

	// Keyboard.
	key       string
	code      string
	vk        int
	text      string
	modifiers int

	// Navigation.
	url   string
	delta int

	// Tab operations carry the MASTER's target id; the worker
	// resolves it through its own binding table. Positions are
	// never used: CDP target lists are not tab-strip order.
	masterTarget string

	// evUIOp carries one semantic browser-chrome operation.
	uiOp *uiOpCommand

	// evRefreshViewport carries the master's current viewport
	// size; the worker re-reads its own.
	masterW, masterH float64

	// evInit bootstraps the worker: the verified master ->
	// follower target bindings and the master's current target.
	bindings      map[string]string
	currentTarget string
}

// uiOpCommand is one semantic browser-chrome operation for a
// follower to execute through UI Automation. Label is the
// user-facing operation name used in failure feedback. No text
// values are ever carried: UIA sync never transfers typed text.
type uiOpCommand struct {
	Action string // invoke | expand | collapse | toggle | select
	Spec   uia.MatchSpec
	Label  string
}

// opFailure marks an operation that could not be performed for
// THIS follower but does not end its participation (an unbound
// tab activation, a missing UIA control). It is reported to the
// user per follower; the follower keeps syncing everything else.
type opFailure struct {
	op     string
	reason string
}

func (e *opFailure) Error() string { return e.reason }

// ---- follower queue -------------------------------------------------

type pushResult int

const (
	pushOK pushResult = iota
	pushCoalesced
	pushDropped // a safely-droppable move was dropped
	pushOverload
)

// followerQueue is one follower's bounded event queue. All
// methods are safe for concurrent use; producers never block.
//
// Ordering rules (Stage 10 v2 review fix):
//   - A mouse move may only replace the queue's TAIL when the
//     tail is itself a move — the trailing run of consecutive
//     moves collapses to the latest position. A move separated
//     from the tail by any other event is never touched.
//   - When the queue is full, the only events that may be
//     evicted to make room are mouse moves (stateless positions).
//   - A critical event that cannot be queued after evicting
//     moves reports pushOverload; the caller fails the follower
//     and releases its input state rather than dropping the
//     event and corrupting press/release pairing.
type followerQueue struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []syncEvent
	cap     int
	closed  bool
	dropped int
}

func newFollowerQueue(capacity int) *followerQueue {
	q := &followerQueue{cap: capacity}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *followerQueue) push(ev syncEvent) pushResult {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return pushDropped
	}
	if ev.kind == evMouseMove && len(q.items) > 0 &&
		q.items[len(q.items)-1].kind == evMouseMove {
		q.items[len(q.items)-1] = ev
		return pushCoalesced
	}
	if len(q.items) >= q.cap {
		if !q.evictOldestMove() {
			if ev.kind == evMouseMove {
				q.dropped++
				return pushDropped
			}
			return pushOverload
		}
	}
	q.items = append(q.items, ev)
	q.cond.Signal()
	return pushOK
}

// pushFront inserts a control event (release/stop) ahead of
// pending input so pause/stop take effect immediately. Control
// events are always admitted, even past capacity.
func (q *followerQueue) pushFront(ev syncEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed && ev.kind != evStop {
		return
	}
	q.items = append([]syncEvent{ev}, q.items...)
	q.cond.Signal()
}

// evictOldestMove removes the oldest mouse move to make room.
// Moves are the only safely evictable events: they carry no
// state transitions. Caller holds q.mu.
func (q *followerQueue) evictOldestMove() bool {
	for i := range q.items {
		if q.items[i].kind == evMouseMove {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.dropped++
			return true
		}
	}
	return false
}

// clearInput drops pending input events (pause semantics: stale
// input is never replayed), keeping control events.
func (q *followerQueue) clearInput() {
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	for _, ev := range q.items {
		if ev.kind == evRelease || ev.kind == evStop || ev.kind == evRefreshViewport {
			kept = append(kept, ev)
		} else {
			q.dropped++
		}
	}
	q.items = kept
}

func (q *followerQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// pop waits for the next event; ok=false when closed and drained.
func (q *followerQueue) pop() (syncEvent, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.items) == 0 {
		return syncEvent{}, false
	}
	ev := q.items[0]
	q.items = q.items[1:]
	return ev, true
}

func (q *followerQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// ---- follower worker ------------------------------------------------

// followerWorker owns one follower instance's CDP connection for
// a sync session.
type followerWorker struct {
	id     string
	client *cdp.Client
	queue  *followerQueue
	hwnd   uintptr // the follower's top window, for UIA ops

	// uiExec executes one semantic chrome operation; it is a
	// field so tests can substitute a fake executor.
	uiExec func(hwnd uintptr, op uia.UIOp) error

	// Owned by the worker goroutine only.
	session       string
	bindings      map[string]string // master target id -> follower target id
	currentMaster string
	viewport      struct{ w, h float64 }
	master        struct{ w, h float64 }
	lastX         float64
	lastY         float64

	pressedKeys    map[int]cdp.KeyEvent
	pressedButtons map[string]bool

	mu         sync.Mutex
	failed     bool
	failReason string
	onFail     func(id string, reason string)
	onOpFail   func(id string, op string, reason string)

	done chan struct{}
}

func newFollowerWorker(id string, client *cdp.Client, onFail func(string, string)) *followerWorker {
	return &followerWorker{
		id:             id,
		client:         client,
		queue:          newFollowerQueue(256),
		uiExec:         uia.ExecuteUIOp,
		bindings:       make(map[string]string),
		pressedKeys:    make(map[int]cdp.KeyEvent),
		pressedButtons: make(map[string]bool),
		onFail:         onFail,
		done:           make(chan struct{}),
	}
}

func (w *followerWorker) isFailed() (bool, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed, w.failReason
}

func (w *followerWorker) markFailed(reason string) {
	w.mu.Lock()
	already := w.failed
	if !already {
		w.failed = true
		w.failReason = reason
	}
	w.mu.Unlock()
	if !already && w.onFail != nil {
		w.onFail(w.id, reason)
	}
}

func (w *followerWorker) callCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

// Enqueue offers an event to this follower. It never blocks. A
// critical event that cannot be queued safely fails the
// follower (with input-state release) instead of being dropped:
// a silently lost press or release would corrupt the follower's
// input state far worse than a clean, reported failure.
func (w *followerWorker) Enqueue(ev syncEvent) {
	if failed, _ := w.isFailed(); failed {
		return
	}
	if w.queue.push(ev) == pushOverload {
		w.markFailed("同步事件过多，已停止该实例的同步")
		w.Release()
	}
}

// Release asks the worker to release all injected input state.
// It is delivered even to failed followers.
func (w *followerWorker) Release() {
	w.queue.pushFront(syncEvent{kind: evRelease})
}

// Stop releases state and ends the worker.
func (w *followerWorker) Stop() {
	w.queue.pushFront(syncEvent{kind: evStop})
}

// RefreshViewport asks the worker to re-read its own viewport
// against the master's new size.
func (w *followerWorker) RefreshViewport(masterW, masterH float64) {
	w.queue.push(syncEvent{kind: evRefreshViewport, masterW: masterW, masterH: masterH})
}

// Run is the worker's dispatch loop — the only goroutine that
// issues commands on this follower's CDP connection (besides
// Close).
func (w *followerWorker) Run() {
	defer close(w.done)
	for {
		ev, ok := w.queue.pop()
		if !ok {
			return
		}
		switch ev.kind {
		case evStop:
			w.releaseAll()
			w.queue.close()
			return
		case evRelease:
			w.releaseAll()
			continue
		}
		if ev.kind == evInit {
			w.bindings = ev.bindings
			if w.bindings == nil {
				w.bindings = make(map[string]string)
			}
			w.master.w, w.master.h = ev.masterW, ev.masterH
			if err := w.attachBound(ev.currentTarget); err != nil {
				w.markFailed("连接跟随实例标签失败：" + err.Error())
			}
			continue
		}
		if failed, _ := w.isFailed(); failed {
			// Input for a failed follower is discarded; control
			// events above still ran.
			continue
		}
		if err := w.execute(ev); err != nil {
			var of *opFailure
			if errors.As(err, &of) {
				// The operation could not be performed for this
				// follower; it is reported, and the follower
				// keeps syncing everything else.
				if w.onOpFail != nil {
					w.onOpFail(w.id, of.op, of.reason)
				}
				continue
			}
			w.markFailed(err.Error())
			// Attempt to leave no pressed state behind on the
			// (possibly still alive) follower.
			w.releaseAll()
		}
	}
}

func (w *followerWorker) execute(ev syncEvent) error {
	switch ev.kind {
	case evRefreshViewport:
		w.master.w, w.master.h = ev.masterW, ev.masterH
		return w.refreshOwnViewport()
	case evMouseMove, evMouseButton, evMouseWheel:
		return w.execMouse(ev)
	case evKey:
		return w.execKey(ev)
	case evNavigate:
		if w.session == "" {
			return nil
		}
		ctx, cancel := w.callCtx()
		defer cancel()
		return w.client.Navigate(ctx, w.session, ev.url)
	case evReload:
		if w.session == "" {
			return nil
		}
		ctx, cancel := w.callCtx()
		defer cancel()
		return w.client.Reload(ctx, w.session)
	case evHistoryDelta:
		return w.execHistoryDelta(ev.delta)
	case evCreateTab:
		return w.execCreateTab(ev.masterTarget)
	case evCloseTab:
		return w.execCloseTab(ev.masterTarget)
	case evActivateTab:
		return w.execActivateTab(ev.masterTarget)
	case evUIOp:
		return w.execUIOp(ev.uiOp)
	}
	return nil
}

func (w *followerWorker) mapPoint(x, y float64) (float64, float64) {
	if w.master.w <= 0 || w.master.h <= 0 || w.viewport.w <= 0 || w.viewport.h <= 0 {
		return x, y
	}
	return x * w.viewport.w / w.master.w, y * w.viewport.h / w.master.h
}

func (w *followerWorker) execMouse(ev syncEvent) error {
	if w.session == "" {
		return nil
	}
	x, y := w.mapPoint(ev.x, ev.y)
	w.lastX, w.lastY = x, y
	ctx, cancel := w.callCtx()
	defer cancel()
	switch ev.kind {
	case evMouseMove:
		return w.client.Mouse(ctx, w.session, cdp.MouseEvent{
			Type: "mouseMoved", X: x, Y: y, Button: "none", Buttons: ev.buttons,
		})
	case evMouseWheel:
		return w.client.Mouse(ctx, w.session, cdp.MouseEvent{
			Type: "mouseWheel", X: x, Y: y, Button: "none", Buttons: ev.buttons,
			DeltaX: ev.deltaX, DeltaY: ev.deltaY,
		})
	case evMouseButton:
		typ := "mouseReleased"
		if ev.down {
			typ = "mousePressed"
		}
		err := w.client.Mouse(ctx, w.session, cdp.MouseEvent{
			Type: typ, X: x, Y: y, Button: ev.button, Buttons: ev.buttons, ClickCount: ev.clickCount,
		})
		if err == nil {
			if ev.down {
				w.pressedButtons[ev.button] = true
			} else {
				delete(w.pressedButtons, ev.button)
			}
		}
		return err
	}
	return nil
}

func (w *followerWorker) execKey(ev syncEvent) error {
	if w.session == "" {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	kev := cdp.KeyEvent{
		Key: ev.key, Code: ev.code, VK: ev.vk, Modifiers: ev.modifiers, Text: ev.text,
	}
	if ev.down {
		kev.Type = "keyDown"
		err := w.client.Key(ctx, w.session, kev)
		if err == nil {
			w.pressedKeys[ev.vk] = kev
		}
		return err
	}
	kev.Type = "keyUp"
	err := w.client.Key(ctx, w.session, kev)
	if err == nil {
		delete(w.pressedKeys, ev.vk)
	}
	return err
}

// execHistoryDelta moves within THIS follower's own navigation
// history by delta entries — back/forward semantics that do not
// assume the follower's stack matches the master's.
func (w *followerWorker) execHistoryDelta(delta int) error {
	if w.session == "" || delta == 0 {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	entries, current, err := w.client.NavigationHistory(ctx, w.session)
	if err != nil {
		return err
	}
	target := current + delta
	if target < 0 || target >= len(entries) {
		return fmt.Errorf("该实例没有可%s的历史记录", map[bool]string{true: "后退", false: "前进"}[delta < 0])
	}
	return w.client.NavigateToHistoryEntry(ctx, w.session, entries[target].ID)
}

// execCreateTab creates the follower's counterpart of a master
// tab and binds the two target ids. It deliberately does NOT
// attach: activation is a separate, verified step, so a
// background tab on the master never steals the follower's
// input session.
func (w *followerWorker) execCreateTab(masterTarget string) error {
	ctx, cancel := w.callCtx()
	defer cancel()
	id, err := w.client.CreateTarget(ctx, "about:blank")
	if err != nil {
		return &opFailure{op: "新建标签", reason: "新建标签失败：" + err.Error()}
	}
	w.bindings[masterTarget] = id
	return nil
}

// execCloseTab closes the follower tab bound to a master tab.
// An unbound master tab has no counterpart to close.
func (w *followerWorker) execCloseTab(masterTarget string) error {
	id, ok := w.bindings[masterTarget]
	if !ok {
		return nil
	}
	delete(w.bindings, masterTarget)
	ctx, cancel := w.callCtx()
	defer cancel()
	if err := w.client.CloseTarget(ctx, id); err != nil {
		return &opFailure{op: "关闭标签", reason: "关闭标签失败：" + err.Error()}
	}
	if masterTarget == w.currentMaster {
		w.session, w.currentMaster = "", ""
	}
	return nil
}

// execActivateTab activates the follower tab bound to a master
// tab. Without a verified binding it refuses: activating a
// guessed tab would send the user's input to the wrong page.
func (w *followerWorker) execActivateTab(masterTarget string) error {
	id, ok := w.bindings[masterTarget]
	if !ok || id == "" {
		return &opFailure{op: "切换标签", reason: "该标签未建立同步映射，无法可靠切换"}
	}
	if masterTarget == w.currentMaster && w.session != "" {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	if err := w.client.ActivateTarget(ctx, id); err != nil {
		return &opFailure{op: "切换标签", reason: "切换标签失败：" + err.Error()}
	}
	return w.attachTo(id, masterTarget)
}

// execUIOp executes one semantic browser-chrome operation on
// this follower's own window. Any failure is an opFailure: it
// is reported for this follower and never faked as success.
func (w *followerWorker) execUIOp(cmd *uiOpCommand) error {
	if cmd == nil {
		return nil
	}
	if w.hwnd == 0 {
		return &opFailure{op: cmd.Label, reason: "找不到该实例的窗口，无法执行界面操作"}
	}
	if err := w.uiExec(w.hwnd, uia.UIOp{Action: cmd.Action, Spec: cmd.Spec}); err != nil {
		return &opFailure{op: cmd.Label, reason: cmd.Label + "未执行：" + err.Error()}
	}
	return nil
}

// attachBound attaches to the follower tab bound to a master
// target (session start).
func (w *followerWorker) attachBound(masterTarget string) error {
	if masterTarget == "" {
		return nil
	}
	id, ok := w.bindings[masterTarget]
	if !ok || id == "" {
		// No verified binding for the master's current tab:
		// input stays unattached until an activation with a
		// binding arrives. Nothing is guessed.
		return nil
	}
	return w.attachTo(id, masterTarget)
}

// attachTo attaches to a follower target and makes it the input
// session.
func (w *followerWorker) attachTo(targetID, masterTarget string) error {
	ctx, cancel := w.callCtx()
	defer cancel()
	sid, err := w.client.Attach(ctx, targetID)
	if err != nil {
		return err
	}
	w.session = sid
	w.currentMaster = masterTarget
	return w.refreshOwnViewport()
}

func (w *followerWorker) refreshOwnViewport() error {
	if w.session == "" {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	vw, vh, err := w.client.LayoutViewport(ctx, w.session)
	if err != nil {
		return err
	}
	if vw > 0 && vh > 0 {
		w.viewport.w, w.viewport.h = vw, vh
	}
	return nil
}

// releaseAll releases every key and mouse button this worker
// injected and has not yet released. It is attempted on pause,
// on stop, after failures, and for failed followers alike; each
// release is independent and a failure is not fatal to the rest.
func (w *followerWorker) releaseAll() {
	if w.session == "" {
		w.pressedKeys = make(map[int]cdp.KeyEvent)
		w.pressedButtons = make(map[string]bool)
		return
	}
	for vk, kev := range w.pressedKeys {
		ctx, cancel := w.callCtx()
		kev.Type = "keyUp"
		_ = w.client.Key(ctx, w.session, kev)
		cancel()
		delete(w.pressedKeys, vk)
	}
	for btn := range w.pressedButtons {
		ctx, cancel := w.callCtx()
		_ = w.client.Mouse(ctx, w.session, cdp.MouseEvent{
			Type: "mouseReleased", X: w.lastX, Y: w.lastY, Button: btn, Buttons: 0,
		})
		cancel()
		delete(w.pressedButtons, btn)
	}
}
