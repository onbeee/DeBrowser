package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
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
//   - continuous mouse moves coalesce to the latest position;
//   - ordered events (press/release, keys, wheel, tabs,
//     navigation) keep their order per follower;
//   - releases are never dropped by the queue;
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

	// Navigation / tabs.
	url   string
	index int
	delta int

	// evRefreshViewport carries the master's current viewport
	// size; the worker re-reads its own.
	masterW, masterH float64
	reply            chan error

	// evInit bootstraps the worker: the follower's page targets
	// in tab order and the index to attach first.
	targets []string
}

func (e syncEvent) isReleaseClass() bool {
	switch e.kind {
	case evRelease, evStop:
		return true
	case evMouseButton:
		return !e.down
	case evKey:
		return !e.down
	}
	return false
}

// followerQueue is one follower's bounded event queue. All
// methods are safe for concurrent use; producers never block.
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

// push enqueues ev without ever blocking the producer (the input
// hook path). Mouse moves coalesce; when the queue is full,
// evictable events (moves first, then oldest non-release events)
// make room; release-class events are only dropped when the
// queue holds nothing but release-class events.
func (q *followerQueue) push(ev syncEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if ev.kind == evMouseMove {
		for i := range q.items {
			if q.items[i].kind == evMouseMove {
				q.items[i] = ev // coalesce: keep only the latest
				return
			}
		}
	}
	if len(q.items) >= q.cap {
		if !q.evictOne(ev.isReleaseClass()) {
			q.dropped++
			return
		}
	}
	q.items = append(q.items, ev)
	q.cond.Signal()
}

// pushFront inserts a control event (release/stop) ahead of
// pending input so pause/stop take effect immediately.
func (q *followerQueue) pushFront(ev syncEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed && ev.kind != evStop {
		return
	}
	q.items = append([]syncEvent{ev}, q.items...)
	q.cond.Signal()
}

// evictOne removes one event to make room. Release-class events
// are evicted only when the incoming event is itself
// release-class and nothing else remains.
func (q *followerQueue) evictOne(incomingRelease bool) bool {
	for i := range q.items {
		if q.items[i].kind == evMouseMove {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.dropped++
			return true
		}
	}
	for i := range q.items {
		if !q.items[i].isReleaseClass() {
			q.items = append(q.items[:i], q.items[i+1:]...)
			q.dropped++
			return true
		}
	}
	if incomingRelease && len(q.items) > 0 {
		q.items = q.items[1:]
		q.dropped++
		return true
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

// followerWorker owns one follower instance's CDP connection for
// a sync session.
type followerWorker struct {
	id     string
	client *cdp.Client
	queue  *followerQueue

	// Owned by the worker goroutine only.
	session  string
	targets  []string // page target ids in tab order
	current  int      // index into targets, -1 when none
	viewport struct{ w, h float64 }
	master   struct{ w, h float64 }
	lastX    float64
	lastY    float64

	pressedKeys    map[int]cdp.KeyEvent
	pressedButtons map[string]bool

	mu         sync.Mutex
	failed     bool
	failReason string
	onFail     func(id string, reason string)

	done chan struct{}
}

func newFollowerWorker(id string, client *cdp.Client, onFail func(string, string)) *followerWorker {
	return &followerWorker{
		id:             id,
		client:         client,
		queue:          newFollowerQueue(256),
		current:        -1,
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

// Enqueue offers an input event to this follower. It never
// blocks and silently drops when the follower has failed (the
// release path is separate and always attempted).
func (w *followerWorker) Enqueue(ev syncEvent) {
	if failed, _ := w.isFailed(); failed {
		return
	}
	w.queue.push(ev)
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
			w.targets = ev.targets
			w.master.w, w.master.h = ev.masterW, ev.masterH
			if err := w.attachCurrent(ev.index); err != nil {
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
		return w.execCreateTab(ev.index, ev.url)
	case evCloseTab:
		return w.execCloseTab(ev.index)
	case evActivateTab:
		return w.execActivateTab(ev.index)
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

func (w *followerWorker) execCreateTab(index int, url string) error {
	ctx, cancel := w.callCtx()
	defer cancel()
	if url == "" {
		url = "about:blank"
	}
	id, err := w.client.CreateTarget(ctx, url)
	if err != nil {
		return err
	}
	if index < 0 || index > len(w.targets) {
		index = len(w.targets)
	}
	w.targets = append(w.targets, "")
	copy(w.targets[index+1:], w.targets[index:])
	w.targets[index] = id
	return w.attachCurrent(index)
}

func (w *followerWorker) execCloseTab(index int) error {
	if index < 0 || index >= len(w.targets) {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	id := w.targets[index]
	w.targets = append(w.targets[:index], w.targets[index+1:]...)
	if err := w.client.CloseTarget(ctx, id); err != nil {
		return err
	}
	if len(w.targets) == 0 {
		w.session, w.current = "", -1
		return nil
	}
	if w.current >= len(w.targets) {
		w.current = len(w.targets) - 1
	}
	if index <= w.current {
		return w.attachCurrent(w.current)
	}
	return nil
}

func (w *followerWorker) execActivateTab(index int) error {
	if index < 0 || index >= len(w.targets) || index == w.current {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	if err := w.client.ActivateTarget(ctx, w.targets[index]); err != nil {
		return err
	}
	return w.attachCurrent(index)
}

// attachCurrent attaches to targets[index] and makes it the
// input session.
func (w *followerWorker) attachCurrent(index int) error {
	if index < 0 || index >= len(w.targets) {
		return nil
	}
	ctx, cancel := w.callCtx()
	defer cancel()
	sid, err := w.client.Attach(ctx, w.targets[index])
	if err != nil {
		return err
	}
	w.session = sid
	w.current = index
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
