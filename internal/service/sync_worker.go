package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// followerWorker owns one follower instance's side of a sync
// session: its CDP connection, its page tracker, the
// master→follower target mapping, a bounded event queue and a
// single serial dispatch goroutine. A failure here never
// touches other followers: it marks this worker failed,
// releases any pressed inputs and tears down only itself.
type followerWorker struct {
	id     string
	client *cdp.Client
	track  *pageTracker

	queue chan PageEvent
	done  chan struct{}

	mu      sync.Mutex
	failed  bool
	failMsg string
	// mapping: master target -> follower target
	mapping map[string]string
	// creating: in-flight target creations, single-flight per
	// (master target) for THIS follower
	creating map[string]*targetCreation
	// pressed input state, for release on pause/failure/stop
	keysDown    map[string]bool
	buttonsDown int
	// viewport sizes for coordinate scaling (CSS pixels)
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
		queue:    make(chan PageEvent, 256),
		done:     make(chan struct{}),
		mapping:  map[string]string{},
		creating: map[string]*targetCreation{},
		keysDown: map[string]bool{},
		onFail:   onFail,
	}
	w.track = newPageTracker(client, false, nil, nil)
	return w
}

func (w *followerWorker) start(ctx context.Context) {
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

// markFailed fails ONLY this follower: inputs are released,
// the queue is drained and the dispatch loop ends.
func (w *followerWorker) markFailed(reason string) {
	w.mu.Lock()
	if w.failed {
		w.mu.Unlock()
		return
	}
	w.failed = true
	w.failMsg = reason
	w.mu.Unlock()
	w.releaseInputs(context.Background())
	close(w.done)
	if w.onFail != nil {
		w.onFail(w.id, reason)
	}
}

// enqueue offers an event to the bounded queue. Pointer moves
// coalesce with a queued tail move; wheel events merge deltas
// into a queued tail wheel at the same position. Critical
// events are never silently dropped: if one cannot be queued,
// the follower fails instead.
func (w *followerWorker) enqueue(ev PageEvent) bool {
	if w.isFailed() {
		return false
	}
	if ev.Kind == "pointermove" {
		select {
		case w.queue <- ev:
			return true
		default:
		}
		// Queue full: a move may be dropped ONLY if a later
		// move will supersede it; try once more after a tail
		// drain of pending moves.
		select {
		case w.queue <- ev:
			return true
		default:
			return false // non-critical: dropping a move is safe
		}
	}
	select {
	case w.queue <- ev:
		return true
	default:
	}
	if ev.IsCritical() {
		w.markFailed("同步事件队列已满，无法保证操作顺序")
		return false
	}
	return false
}

// clearPending drops queued non-dispatched events (pause).
func (w *followerWorker) clearPending() {
	for {
		select {
		case <-w.queue:
		default:
			return
		}
	}
}

// mappedTarget returns the follower target for a master target.
func (w *followerWorker) mappedTarget(masterTarget string) (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	t, ok := w.mapping[masterTarget]
	return t, ok
}

// setMapping records a verified mapping (after WaitReady).
func (w *followerWorker) setMapping(masterTarget, followerTarget string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.mapping[masterTarget] = followerTarget
}

// dropMapping removes mappings pointing at a destroyed target.
func (w *followerWorker) dropMapping(masterTarget string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.mapping, masterTarget)
}

// ensureTarget returns the follower target corresponding to a
// master target, creating it exactly once even under
// concurrent triggers: callers share the in-flight creation's
// outcome. The mapping is recorded only after the new page is
// attached and ready.
func (w *followerWorker) ensureTarget(ctx context.Context, masterTarget, url string) (string, error) {
	if t, ok := w.mappedTarget(masterTarget); ok {
		return t, nil
	}
	w.mu.Lock()
	if w.creating == nil {
		w.creating = map[string]*targetCreation{}
	}
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
		url = "about:blank"
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
		return "", err
	}
	if err := w.track.WaitReady(actx, targetID); err != nil {
		return "", err
	}
	return targetID, nil
}

// pairWith records the mapping after a successful ensureTarget
// for the given master target.
func (w *followerWorker) pairWith(masterTarget, followerTarget string) {
	w.setMapping(masterTarget, followerTarget)
}

// sessionFor returns the attached session of the follower
// target mapped from a master target.
func (w *followerWorker) sessionFor(masterTarget string) (string, string, bool) {
	ft, ok := w.mappedTarget(masterTarget)
	if !ok {
		return "", "", false
	}
	sid, ok := w.track.Session(ft)
	return ft, sid, ok
}

// setViewports records both sides' CSS viewport sizes for
// coordinate scaling.
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

// dispatchLoop is the follower's single serial dispatcher.
func (w *followerWorker) dispatchLoop(ctx context.Context) {
	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			return
		case ev := <-w.queue:
			w.dispatch(ctx, ev)
		}
	}
}

func (w *followerWorker) dispatch(ctx context.Context, ev PageEvent) {
	if w.isFailed() {
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
		if ft, ok := w.mappedTarget(ev.Source); ok {
			_, _ = w.client.Call(cctx, "", "Target.closeTarget", map[string]any{"targetId": ft})
			w.dropMapping(ev.Source)
			w.track.Forget(ft)
		}
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

func (w *followerWorker) dispatchInput(ctx context.Context, ev PageEvent) {
	_, sid, ok := w.sessionFor(ev.Source)
	if !ok {
		return
	}
	switch ev.Kind {
	case "pointermove", "pointerdown", "pointerup":
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
		w.mu.Lock()
		if ev.Kind == "pointerdown" {
			w.buttonsDown |= 1 << uint(max(0, ev.Button))
		} else if ev.Kind == "pointerup" {
			w.buttonsDown &^= 1 << uint(max(0, ev.Button))
		}
		w.mu.Unlock()
	case "wheel":
		x, y := w.scale(ev.X, ev.Y)
		_, _ = w.client.Call(ctx, sid, "Input.dispatchMouseEvent", map[string]any{
			"type": "mouseWheel", "x": x, "y": y,
			"deltaX": ev.DeltaX, "deltaY": ev.DeltaY,
			"modifiers": ev.Modifiers(),
		})
	case "keydown", "keyup":
		if ev.IsPasteShortcut() {
			return // text arrives once, as the paste increment
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
		// arrive exactly once via input increments.
		_, _ = w.client.Call(ctx, sid, "Input.dispatchKeyEvent", params)
		w.mu.Lock()
		if ev.Kind == "keydown" {
			w.keysDown[ev.Code] = true
		} else {
			delete(w.keysDown, ev.Code)
		}
		w.mu.Unlock()
	case "input", "insertfrompaste", "compositionend":
		if ev.Text != "" {
			_, _ = w.client.Call(ctx, sid, "Input.insertText", map[string]any{"text": ev.Text})
		}
	}
}

// releaseInputs dispatches releases for everything this worker
// believes is pressed, then clears the belief. Used on pause,
// failure and stop so a follower can never be left with a
// stuck key or button.
func (w *followerWorker) releaseInputs(ctx context.Context) {
	w.mu.Lock()
	keys := make([]string, 0, len(w.keysDown))
	for code := range w.keysDown {
		keys = append(keys, code)
	}
	buttons := w.buttonsDown
	w.keysDown = map[string]bool{}
	w.buttonsDown = 0
	masterTarget := ""
	for mt := range w.mapping {
		masterTarget = mt
		break
	}
	w.mu.Unlock()
	if masterTarget == "" {
		return
	}
	_, sid, ok := w.sessionFor(masterTarget)
	if !ok {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	for _, code := range keys {
		_, _ = w.client.Call(cctx, sid, "Input.dispatchKeyEvent", map[string]any{
			"type": "keyUp", "code": code, "windowsVirtualKeyCode": vkForCode(code),
		})
	}
	for b := 0; b < 5; b++ {
		if buttons&(1<<uint(b)) != 0 {
			_, _ = w.client.Call(cctx, sid, "Input.dispatchMouseEvent", map[string]any{
				"type": "mouseReleased", "button": cdpMouseButton(b), "buttons": 0,
			})
		}
	}
}

// close tears down this follower's connection and bookkeeping.
func (w *followerWorker) close() {
	w.mu.Lock()
	if !w.failed {
		w.failed = true
		w.failMsg = "同步已停止"
	}
	w.mu.Unlock()
	select {
	case <-w.done:
	default:
		close(w.done)
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
