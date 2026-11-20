package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// syncQueue is one follower's bounded event queue with tail
// coalescing: a pointer move replaces a still-queued tail move
// for the same source target, and a wheel event merges its
// deltas into a still-queued tail wheel for the same source
// (direction and order preserved, latest position kept).
// Critical events are never silently dropped: when one cannot
// be queued, push reports overflow and the worker fails.
type syncQueue struct {
	mu     sync.Mutex
	items  []PageEvent
	cap    int
	notify chan struct{}
}

func newSyncQueue(capacity int) *syncQueue {
	return &syncQueue{cap: capacity, notify: make(chan struct{}, 1)}
}

func (q *syncQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

// push queues one event; overflow=true means a CRITICAL event
// could not be queued and the follower must fail.
func (q *syncQueue) push(ev PageEvent) (overflow bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if n := len(q.items); n > 0 {
		last := &q.items[n-1]
		if ev.Kind == "pointermove" && last.Kind == "pointermove" && last.Source == ev.Source {
			*last = ev
			return false
		}
		if ev.Kind == "wheel" && last.Kind == "wheel" && last.Source == ev.Source {
			last.DeltaX += ev.DeltaX
			last.DeltaY += ev.DeltaY
			last.X, last.Y = ev.X, ev.Y
			return false
		}
	}
	if len(q.items) >= q.cap {
		return ev.IsCritical()
	}
	q.items = append(q.items, ev)
	q.signal()
	return false
}

// pop removes the head event, waiting briefly for one to
// arrive. ok=false means "nothing right now".
func (q *syncQueue) pop(wait time.Duration) (PageEvent, bool) {
	q.mu.Lock()
	if len(q.items) > 0 {
		ev := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		return ev, true
	}
	q.mu.Unlock()
	select {
	case <-q.notify:
	case <-time.After(wait):
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.items) == 0 {
		return PageEvent{}, false
	}
	ev := q.items[0]
	q.items = q.items[1:]
	return ev, true
}

func (q *syncQueue) clear() {
	q.mu.Lock()
	q.items = nil
	q.mu.Unlock()
}

func (q *syncQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// targetInputs is the pressed-input state of ONE follower
// target: releases must go to the very target that received
// the presses, never to an arbitrary mapped target.
type targetInputs struct {
	keys    map[string]bool
	buttons int
}

// followerWorker owns one follower instance's side of a sync
// session: its CDP connection, its page tracker, the
// master→follower target mapping, a bounded coalescing queue
// and a single serial dispatch goroutine. ALL CDP input —
// regular dispatch and input releases alike — is issued by
// that one goroutine, so a release can never race a press.
// A failure here never touches other followers.
type followerWorker struct {
	id     string
	client *cdp.Client
	track  *pageTracker

	queue    *syncQueue
	control  chan struct{} // input-release requests (buffered 1)
	stopCh   chan struct{}
	loopDone chan struct{}
	started  bool
	stopOnce sync.Once

	mu       sync.Mutex
	failed   bool
	failMsg  string
	mapping  map[string]string
	creating map[string]*targetCreation
	inputs   map[string]*targetInputs // follower target -> pressed state

	masterVP   [2]float64
	followerVP [2]float64
	vpKnown    bool

	onFail func(id, reason string)
}

type targetCreation struct {
	done   chan struct{}
	target string
	err    error
}

func newFollowerWorker(id string, client *cdp.Client, onFail func(id, reason string)) *followerWorker {
	w := &followerWorker{
		id:       id,
		client:   client,
		queue:    newSyncQueue(256),
		control:  make(chan struct{}, 1),
		stopCh:   make(chan struct{}),
		loopDone: make(chan struct{}),
		mapping:  map[string]string{},
		creating: map[string]*targetCreation{},
		inputs:   map[string]*targetInputs{},
		onFail:   onFail,
	}
	w.track = newPageTracker(client, false, nil, nil)
	return w
}

func (w *followerWorker) start(ctx context.Context) {
	w.started = true
	go w.dispatchLoop(ctx)
	go func() {
		<-w.client.Done()
		w.markFailed("跟随实例的调试连接已断开")
	}()
}

func (w *followerWorker) isFailed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (w *followerWorker) failureReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failMsg
}

// markFailed fails ONLY this follower. It never issues CDP
// itself: the release of pressed inputs is requested through
// the control channel and executed by the dispatch goroutine.
func (w *followerWorker) markFailed(reason string) {
	w.mu.Lock()
	if w.failed {
		w.mu.Unlock()
		return
	}
	w.failed = true
	w.failMsg = reason
	w.mu.Unlock()
	w.requestRelease()
	if w.onFail != nil {
		w.onFail(w.id, reason)
	}
}

// requestRelease asks the dispatch goroutine to release every
// pressed input on every target. Non-blocking.
func (w *followerWorker) requestRelease() {
	select {
	case w.control <- struct{}{}:
	default:
	}
}

// requestPurgeAndRelease drops all queued events and releases
// pressed inputs (pause semantics: nothing is replayed later).
func (w *followerWorker) requestPurgeAndRelease() {
	w.queue.clear()
	w.requestRelease()
}

// clearPending drops queued events without releasing inputs.
func (w *followerWorker) clearPending() { w.queue.clear() }

// enqueue offers an event to the bounded queue. A critical
// event that cannot be queued fails the follower instead of
// being silently dropped.
func (w *followerWorker) enqueue(ev PageEvent) bool {
	if w.isFailed() {
		return false
	}
	if w.queue.push(ev) {
		w.markFailed("同步事件队列已满，无法保证操作顺序")
		return false
	}
	return true
}

func (w *followerWorker) mappedTarget(masterTarget string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.mapping[masterTarget]
	return t, ok
}

// mappedMasterTargets snapshots the master targets this
// follower currently has counterparts for.
func (w *followerWorker) mappedMasterTargets() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.mapping))
	for mt := range w.mapping {
		out = append(out, mt)
	}
	return out
}

func (w *followerWorker) setMapping(masterTarget, followerTarget string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mapping[masterTarget] = followerTarget
}

func (w *followerWorker) dropMapping(masterTarget string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.mapping, masterTarget)
}

// ensureTarget returns the follower target corresponding to a
// master target, creating it exactly once even under
// concurrent triggers: callers share the in-flight creation's
// outcome. The mapping is recorded only after the new page is
// attached and ready; a creation that fails midway closes the
// half-created target so no stray tab is left behind.
func (w *followerWorker) ensureTarget(ctx context.Context, masterTarget, url string) (string, error) {
	if t, ok := w.mappedTarget(masterTarget); ok {
		return t, nil
	}
	w.mu.Lock()
	if c, ok := w.creating[masterTarget]; ok {
		w.mu.Unlock()
		select {
		case <-c.done:
			return c.target, c.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	c := &targetCreation{done: make(chan struct{})}
	w.creating[masterTarget] = c
	w.mu.Unlock()

	target, err := w.createTarget(ctx, url)

	w.mu.Lock()
	delete(w.creating, masterTarget)
	c.target, c.err = target, err
	close(c.done)
	w.mu.Unlock()
	return target, err
}

func (w *followerWorker) createTarget(ctx context.Context, url string) (string, error) {
	if url == "" {
		// Never materialize a creation-instant empty URL as
		// a permanent about:blank tab: the session offers a
		// target for creation only once its URL is known
		// (Target.targetInfoChanged). An empty URL here is a
		// programming error, reported — not guessed.
		return "", fmt.Errorf("标签页地址尚未就绪")
	}
	targetID, err := w.client.CallString(ctx, "", "Target.createTarget", map[string]any{
		"url": url, "background": true,
	}, "targetId")
	if err != nil {
		return "", err
	}
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := w.track.Attach(actx, targetID); err != nil {
		// The target exists but could not be taken over:
		// close it rather than leaving a stray tab behind.
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = w.client.Call(cctx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
		ccancel()
		return "", err
	}
	if err := w.track.WaitReady(actx, targetID); err != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = w.client.Call(cctx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
		ccancel()
		return "", err
	}
	return targetID, nil
}

func (w *followerWorker) pairWith(masterTarget, followerTarget string) {
	w.setMapping(masterTarget, followerTarget)
}

// sessionFor returns the follower target and session mapped
// from a master target.
func (w *followerWorker) sessionFor(masterTarget string) (string, string, bool) {
	ft, ok := w.mappedTarget(masterTarget)
	if !ok {
		return "", "", false
	}
	sid, ok := w.track.Session(ft)
	return ft, sid, ok
}

func (w *followerWorker) setViewports(masterW, masterH, followerW, followerH float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.masterVP = [2]float64{masterW, masterH}
	w.followerVP = [2]float64{followerW, followerH}
	w.vpKnown = masterW > 0 && masterH > 0 && followerW > 0 && followerH > 0
}

func (w *followerWorker) scale(x, y float64) (float64, float64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.vpKnown {
		return x, y
	}
	return x * w.followerVP[0] / w.masterVP[0], y * w.followerVP[1] / w.masterVP[1]
}

// dispatchLoop is the follower's single serial dispatcher and
// the ONLY goroutine that issues input commands for this
// follower. Release requests (control channel) are handled
// between events, so they can never race a dispatch.
func (w *followerWorker) dispatchLoop(ctx context.Context) {
	defer close(w.loopDone)
	for {
		select {
		case <-w.stopCh:
			w.drainPendingCloses()
			return
		case <-ctx.Done():
			w.drainPendingCloses()
			return
		case <-w.control:
			w.releaseAllInputs(ctx)
			continue
		default:
		}
		ev, ok := w.queue.pop(50 * time.Millisecond)
		if !ok {
			continue
		}
		// A release request that arrived while waiting takes
		// priority over further queued events.
		select {
		case <-w.control:
			w.releaseAllInputs(ctx)
			if w.isFailed() && ev.Kind != "targetclosed" {
				continue
			}
		default:
		}
		w.dispatch(ctx, ev)
	}
}

func (w *followerWorker) dispatch(ctx context.Context, ev PageEvent) {
	// A failed or closing worker takes no new input — but a
	// queued tab closure is still honored: it is the
	// session's teardown bookkeeping, not input, and
	// dropping it would leave a mapped tab behind on the
	// follower (its closeTarget simply errors if this
	// follower's own connection is already gone).
	if w.isFailed() && ev.Kind != "targetclosed" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	switch ev.Kind {
	case "targetcreated":
		ft, err := w.ensureTarget(cctx, ev.Source, ev.Text)
		if err != nil {
			// A single target that cannot be created is
			// skipped (no mapping, no guessing); the
			// follower itself stays in the session and a
			// later activation refresh may retry.
			return
		}
		w.pairWith(ev.Source, ft)
	case "targetclosed":
		w.closeMappedTarget(cctx, ev.Source)
	case "targetactivated":
		if ft, ok := w.mappedTarget(ev.Source); ok {
			_, _ = w.client.Call(cctx, "", "Target.activateTarget", map[string]any{"targetId": ft})
		}
	case "navigate":
		if _, sid, ok := w.sessionFor(ev.Source); ok {
			_, _ = w.client.Call(cctx, sid, "Page.navigate", map[string]any{"url": ev.Text})
		}
	case "reload":
		if _, sid, ok := w.sessionFor(ev.Source); ok {
			_, _ = w.client.Call(cctx, sid, "Page.reload", nil)
		}
	case "historyback", "historyforward":
		w.navigateHistory(cctx, ev)
	default:
		w.dispatchInput(cctx, ev)
	}
}

// closeMappedTarget closes the follower counterpart of one
// master target: its pressed inputs are released FIRST, while
// its session still exists, then the target is closed and the
// mapping dropped. A master target with no mapping is a
// no-op — which counterpart to close is never guessed from
// order, URL or the active tab — so a duplicate destruction
// event can never close the wrong tab.
func (w *followerWorker) closeMappedTarget(ctx context.Context, masterTarget string) {
	ft, ok := w.mappedTarget(masterTarget)
	if !ok {
		return
	}
	w.releaseTargetInputs(ctx, ft)
	_, _ = w.client.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": ft})
	w.dropMapping(masterTarget)
	w.track.Forget(ft)
}

// drainPendingCloses runs on the dispatch goroutine as it
// exits (session stop, or the master connection dying with
// the session): tab closures that are already queued are
// still executed — in queue order — instead of being
// discarded with the loop, and every remaining pressed input
// is released afterwards. Other queued events are NOT
// replayed: stop semantics stay "nothing is replayed".
// This is what lets the follower close its counterpart of
// the master's LAST tab even when the master's CDP
// connection drops immediately after the destruction event.
func (w *followerWorker) drainPendingCloses() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		ev, ok := w.queue.pop(0)
		if !ok {
			break
		}
		if ev.Kind == "targetclosed" {
			w.closeMappedTarget(ctx, ev.Source)
		}
	}
	w.releaseAllInputs(ctx)
}

// initTargets establishes the initial one-to-one mapping for
// ALL of the master's pre-existing page targets, before any
// event dispatch begins. Counterparts are resolved by
// planInitialMapping: the follower's own pre-existing tabs
// are reused first (attached and confirmed ready before the
// mapping is recorded), and a new tab is created only for a
// master tab with no reusable counterpart. On failure every
// tab THIS initialization created is closed again — reused
// tabs are the user's own and are never closed — and no
// mapping is left behind.
func (w *followerWorker) initTargets(ctx context.Context, master []pageTargetInfo, activeMaster string) error {
	follower, err := listPageTargets(ctx, w.client)
	if err != nil {
		return fmt.Errorf("读取跟随实例已有标签失败：%w", err)
	}
	pairs, toCreate, err := planInitialMapping(master, follower)
	if err != nil {
		return err
	}
	var created []string
	cleanup := func(err error) error {
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer ccancel()
		for _, ft := range created {
			_, _ = w.client.Call(cctx, "", "Target.closeTarget", map[string]any{"targetId": ft})
			w.track.Forget(ft)
		}
		w.mu.Lock()
		w.mapping = map[string]string{}
		w.mu.Unlock()
		return err
	}
	// Reused counterparts, in the master's target order.
	for _, mt := range master {
		ft, ok := pairs[mt.ID]
		if !ok {
			continue
		}
		actx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := w.track.Attach(actx, ft)
		if err == nil {
			err = w.track.WaitReady(actx, ft)
		}
		cancel()
		if err != nil {
			return cleanup(fmt.Errorf("接入跟随实例已有标签失败：%w", err))
		}
		w.pairWith(mt.ID, ft)
	}
	// Master tabs without a reusable counterpart get exactly
	// one newly created counterpart each.
	for _, mt := range toCreate {
		ft, err := w.ensureTarget(ctx, mt.ID, mt.URL)
		if err != nil {
			return cleanup(fmt.Errorf("创建对应标签失败：%w", err))
		}
		created = append(created, ft)
		w.pairWith(mt.ID, ft)
	}
	// The follower shows the counterpart of the master's
	// active tab.
	if activeMaster != "" {
		if ft, ok := w.mappedTarget(activeMaster); ok {
			_, _ = w.client.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": ft})
		}
	}
	return nil
}

func (w *followerWorker) navigateHistory(ctx context.Context, ev PageEvent) {
	_, sid, ok := w.sessionFor(ev.Source)
	if !ok {
		return
	}
	raw, err := w.client.Call(ctx, sid, "Page.getNavigationHistory", nil)
	if err != nil {
		return
	}
	var hist struct {
		CurrentIndex int `json:"currentIndex"`
		Entries      []struct {
			ID int `json:"id"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(raw, &hist); err != nil {
		return
	}
	idx := hist.CurrentIndex
	if ev.Kind == "historyback" {
		idx--
	} else {
		idx++
	}
	if idx < 0 || idx >= len(hist.Entries) {
		return // this follower's own history has no such entry; skip, never guess
	}
	_, _ = w.client.Call(ctx, sid, "Page.navigateToHistoryEntry", map[string]any{
		"entryId": hist.Entries[idx].ID,
	})
}

// inputsFor returns (creating if needed) the pressed-state
// record of one follower target.
func (w *followerWorker) inputsFor(ft string) *targetInputs {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.inputs[ft]
	if !ok {
		st = &targetInputs{keys: map[string]bool{}}
		w.inputs[ft] = st
	}
	return st
}

func (w *followerWorker) dispatchInput(ctx context.Context, ev PageEvent) {
	ft, sid, ok := w.sessionFor(ev.Source)
	if !ok {
		return
	}
	switch ev.Kind {
	case "pointermove", "pointerdown", "pointerup":
		// The right button is never dispatched: its press
		// would open the follower's own native context menu,
		// which is browser UI — not a sync object — and no
		// page event would ever dismiss it (the master's menu
		// interaction happens in native UI and is not
		// captured), leaving a stuck menu on the follower.
		// Left/middle buttons, drags and wheels are
		// unaffected.
		if ev.Kind != "pointermove" && ev.Button == 2 {
			return
		}
		x, y := w.scale(ev.X, ev.Y)
		typ := "mouseMoved"
		if ev.Kind == "pointerdown" {
			typ = "mousePressed"
		} else if ev.Kind == "pointerup" {
			typ = "mouseReleased"
		}
		params := map[string]any{
			"type": typ, "x": x, "y": y,
			"button":    cdpMouseButton(ev.Button),
			"buttons":   ev.Buttons,
			"modifiers": ev.Modifiers(),
		}
		if ev.Kind != "pointermove" {
			params["clickCount"] = 1
		}
		_, _ = w.client.Call(ctx, sid, "Input.dispatchMouseEvent", params)
		if ev.Kind == "pointerdown" {
			w.inputsFor(ft).buttons |= 1 << uint(max(0, ev.Button))
		} else if ev.Kind == "pointerup" {
			w.mu.Lock()
			if st := w.inputs[ft]; st != nil {
				st.buttons &^= 1 << uint(max(0, ev.Button))
			}
			w.mu.Unlock()
		}
	case "wheel":
		x, y := w.scale(ev.X, ev.Y)
		_, _ = w.client.Call(ctx, sid, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y,
			"deltaX": ev.DeltaX, "deltaY": ev.DeltaY,
			"modifiers": ev.Modifiers(),
		})
	case "keydown", "keyup":
		// The dedicated menu key would open the follower's
		// native context menu — same reasoning as the right
		// mouse button above.
		if ev.Code == "ContextMenu" {
			return
		}
		typ := "keyDown"
		if ev.Kind == "keyup" {
			typ = "keyUp"
		}
		params := map[string]any{
			"type": typ, "key": ev.Key, "code": ev.Code,
			"windowsVirtualKeyCode": vkForKey(ev),
			"modifiers":             ev.Modifiers(),
			"isKeypad":              ev.Location == 3,
		}
		if ev.Repeat {
			params["autoRepeat"] = true
		}
		// No text payload on key events by design: characters
		// arrive exactly once via input increments. Paste
		// shortcuts are ordinary key events here — DeBrowser
		// never reads any clipboard; the follower page's own
		// clipboard decides what a paste inserts.
		_, _ = w.client.Call(ctx, sid, "Input.dispatchKeyEvent", params)
		if ev.Kind == "keydown" {
			w.inputsFor(ft).keys[ev.Code] = true
		} else {
			w.mu.Lock()
			if st := w.inputs[ft]; st != nil {
				delete(st.keys, ev.Code)
			}
			w.mu.Unlock()
		}
	case "input", "compositionend":
		if ev.Text != "" {
			_, _ = w.client.Call(ctx, sid, "Input.insertText", map[string]any{"text": ev.Text})
		}
	}
}

// releaseTargetInputs dispatches releases for everything
// pressed on ONE follower target, then clears its record.
// Runs only on the dispatch goroutine.
func (w *followerWorker) releaseTargetInputs(ctx context.Context, ft string) {
	w.mu.Lock()
	st := w.inputs[ft]
	delete(w.inputs, ft)
	w.mu.Unlock()
	if st == nil || (len(st.keys) == 0 && st.buttons == 0) {
		return
	}
	sid, ok := w.track.Session(ft)
	if !ok {
		return // target already gone: nothing can receive releases
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for code := range st.keys {
		_, _ = w.client.Call(cctx, sid, "Input.dispatchKeyEvent", map[string]any{
			"type": "keyUp", "code": code, "windowsVirtualKeyCode": vkForCode(code),
		})
	}
	for b := 0; b < 5; b++ {
		if st.buttons&(1<<uint(b)) != 0 {
			_, _ = w.client.Call(cctx, sid, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "button": cdpMouseButton(b), "buttons": 0,
			})
		}
	}
}

// releaseAllInputs releases every target's pressed inputs.
// Runs only on the dispatch goroutine.
func (w *followerWorker) releaseAllInputs(ctx context.Context) {
	w.mu.Lock()
	targets := make([]string, 0, len(w.inputs))
	for ft := range w.inputs {
		targets = append(targets, ft)
	}
	w.mu.Unlock()
	for _, ft := range targets {
		w.releaseTargetInputs(ctx, ft)
	}
}

// close releases inputs (via the dispatch goroutine), stops
// the loop and closes the connection. Safe on a worker that
// was never started.
func (w *followerWorker) close() {
	w.mu.Lock()
	if !w.failed {
		w.failed = true
		w.failMsg = "同步已停止"
	}
	w.mu.Unlock()
	w.requestRelease()
	if w.started {
		w.stopOnce.Do(func() { close(w.stopCh) })
		select {
		case <-w.loopDone:
		// The loop drains queued tab closures before
		// exiting (bounded by the drain's own 3s context);
		// wait long enough for that drain to finish so the
		// connection is not closed from under it.
		case <-time.After(5 * time.Second):
		}
	}
	w.client.Close()
}

func cdpMouseButton(b int) string {
	switch b {
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
	return "none"
}
