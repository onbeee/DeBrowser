package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/capture"
	"github.com/onbeee/DeBrowser/internal/cdp"
)

// pageTracker attaches to the page targets of ONE browser
// connection (the master's or a follower's), installs the
// observer script on the master side, and tracks which targets
// are ready to receive input. Attach is single-flight per
// target: concurrent triggers share one attach sequence and
// its outcome, and a failed attach leaves nothing behind.
type pageTracker struct {
	client  *cdp.Client
	master  bool
	onEvent func(targetID string, ev PageEvent)
	onReady func(targetID string)

	mu         sync.Mutex
	sessions   map[string]string // targetID -> sessionID
	mainFrames map[string]string // targetID -> main frameID
	contexts   map[string]int    // targetID -> isolated contextID
	attaching  map[string]*attachCall
	ready      map[string]bool
	visible    map[string]bool
	urls       map[string]string
}

type attachCall struct {
	done chan struct{}
	err  error
}

func newPageTracker(client *cdp.Client, master bool, onEvent func(string, PageEvent), onReady func(string)) *pageTracker {
	t := &pageTracker{
		client:     client,
		master:     master,
		onEvent:    onEvent,
		onReady:    onReady,
		sessions:   map[string]string{},
		mainFrames: map[string]string{},
		contexts:   map[string]int{},
		attaching:  map[string]*attachCall{},
		ready:      map[string]bool{},
		visible:    map[string]bool{},
		urls:       map[string]string{},
	}
	if client != nil {
		client.On("Runtime.bindingCalled", t.onBindingCalled)
		client.On("Runtime.executionContextCreated", t.onContextCreated)
		client.On("Page.frameNavigated", t.onFrameNavigated)
	}
	return t
}

// Session returns the attached session for a target.
func (t *pageTracker) Session(targetID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.sessions[targetID]
	return s, ok
}

// IsReady reports whether the target's page is attached and
// its main frame is known.
func (t *pageTracker) IsReady(targetID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready[targetID]
}

// WaitReady blocks until the target is ready or ctx ends.
func (t *pageTracker) WaitReady(ctx context.Context, targetID string) error {
	for {
		if t.IsReady(targetID) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// VisibleTarget returns the unique target currently reporting
// itself visible, or "" when there is none or more than one —
// callers must treat "" as "cannot confirm", never guess.
func (t *pageTracker) VisibleTarget() string {
	id, count := t.visibleState()
	if count != 1 {
		return ""
	}
	return id
}

// visibleState reports the currently visible targets as a
// count plus the single visible target when the count is
// exactly one. "None visible" (a covered or minimized window,
// or reports not yet arrived) and "several visible" (which
// must never be guessed between) stay distinguishable for
// callers and tests, even though both mean "cannot confirm"
// for activation purposes.
func (t *pageTracker) visibleState() (string, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	found := ""
	count := 0
	for id, v := range t.visible {
		if !v {
			continue
		}
		count++
		if found == "" {
			found = id
		}
	}
	if count != 1 {
		return "", count
	}
	return found, count
}

// mainFrameOf returns the tracked main frame ID of a target.
func (t *pageTracker) mainFrameOf(targetID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.mainFrames[targetID]
}

// URL returns the last known main-frame URL of a target
// (session memory only; never logged).
func (t *pageTracker) URL(targetID string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.urls[targetID]
}

// Forget drops all bookkeeping for a destroyed target.
func (t *pageTracker) Forget(targetID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, targetID)
	delete(t.mainFrames, targetID)
	delete(t.contexts, targetID)
	delete(t.ready, targetID)
	delete(t.visible, targetID)
	delete(t.urls, targetID)
}

// Attach attaches to a page target exactly once per target at
// a time. Followers only attach (no script, no binding); the
// master additionally enables Page/Runtime, registers the
// report binding and installs the observer script.
func (t *pageTracker) Attach(ctx context.Context, targetID string) error {
	t.mu.Lock()
	if _, ok := t.sessions[targetID]; ok {
		t.mu.Unlock()
		return nil
	}
	if call, ok := t.attaching[targetID]; ok {
		t.mu.Unlock()
		select {
		case <-call.done:
			return call.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	call := &attachCall{done: make(chan struct{})}
	t.attaching[targetID] = call
	t.mu.Unlock()

	err := t.attach(ctx, targetID)

	t.mu.Lock()
	delete(t.attaching, targetID)
	call.err = err
	close(call.done)
	if err != nil {
		delete(t.sessions, targetID)
		delete(t.mainFrames, targetID)
		delete(t.contexts, targetID)
		delete(t.ready, targetID)
	}
	t.mu.Unlock()
	return err
}

func (t *pageTracker) attach(ctx context.Context, targetID string) (err error) {
	raw, err := t.client.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &attached); err != nil || attached.SessionID == "" {
		return fmt.Errorf("附加到页面失败")
	}
	sid := attached.SessionID
	t.mu.Lock()
	t.sessions[targetID] = sid
	t.mu.Unlock()
	// Any failure from here on must give the session back:
	// detach it and drop the bookkeeping, so a half-attached
	// target never lingers.
	defer func() {
		if err != nil {
			dctx, dcancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = t.client.Call(dctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sid})
			dcancel()
			t.mu.Lock()
			delete(t.sessions, targetID)
			delete(t.mainFrames, targetID)
			delete(t.contexts, targetID)
			delete(t.ready, targetID)
			t.mu.Unlock()
		}
	}()

	if _, err := t.client.Call(ctx, sid, "Page.enable", nil); err != nil {
		return err
	}
	if _, err := t.client.Call(ctx, sid, "Runtime.enable", nil); err != nil {
		return err
	}
	if t.master {
		if _, err := t.client.Call(ctx, sid, "Runtime.addBinding", map[string]any{"name": capture.BindingName}); err != nil {
			return err
		}
		if _, err := t.client.Call(ctx, sid, "Page.addScriptToEvaluateOnNewDocument", map[string]any{
			"source": capture.Script, "worldName": "debrowser-sync",
		}); err != nil {
			return err
		}
	}
	// Learn the main frame so readiness can be established.
	raw, err = t.client.Call(ctx, sid, "Page.getFrameTree", nil)
	if err != nil {
		return err
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil || tree.FrameTree.Frame.ID == "" {
		return fmt.Errorf("读取页面框架失败")
	}
	t.mu.Lock()
	t.mainFrames[targetID] = tree.FrameTree.Frame.ID
	t.ready[targetID] = true
	t.mu.Unlock()
	if t.master {
		// The observer MUST be live in the already-loaded
		// document: without an event source the session
		// would report success while syncing nothing, so a
		// failure here fails the attach (and with it, for
		// the master, the whole StartSync).
		if err := t.installInExistingWorld(ctx, targetID, sid); err != nil {
			return fmt.Errorf("在主实例页面安装观察脚本失败：%w", err)
		}
	}
	if t.onReady != nil {
		go t.onReady(targetID)
	}
	return nil
}

// installInExistingWorld evaluates the observer script in an
// isolated world of the page that already exists (the script
// registered for new documents does not run in documents that
// predate the attach).
func (t *pageTracker) installInExistingWorld(ctx context.Context, targetID, sid string) error {
	t.mu.Lock()
	frameID := t.mainFrames[targetID]
	t.mu.Unlock()
	if frameID == "" {
		return fmt.Errorf("页面主框架未知")
	}
	raw, err := t.client.Call(ctx, sid, "Page.createIsolatedWorld", map[string]any{
		"frameId": frameID, "worldName": "debrowser-sync", "grantUniveralAccess": false,
	})
	if err != nil {
		return err
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := json.Unmarshal(raw, &world); err != nil || world.ExecutionContextID == 0 {
		return fmt.Errorf("创建页面隔离执行环境失败")
	}
	t.mu.Lock()
	t.contexts[targetID] = world.ExecutionContextID
	t.mu.Unlock()
	if _, err := t.client.Call(ctx, sid, "Runtime.evaluate", map[string]any{
		"expression": capture.Script, "contextId": world.ExecutionContextID,
	}); err != nil {
		return err
	}
	return nil
}

func (t *pageTracker) onContextCreated(ev cdp.Event) {
	if !t.master {
		return
	}
	var p struct {
		Context struct {
			ID      int    `json:"id"`
			Name    string `json:"name"`
			AuxData struct {
				FrameID string `json:"frameId"`
				IsMain  bool   `json:"isMain"`
			} `json:"auxData"`
		} `json:"context"`
	}
	if err := json.Unmarshal(ev.Params, &p); err != nil {
		return
	}
	if p.Context.Name != "debrowser-sync" || !p.Context.AuxData.IsMain {
		return // only the top frame's isolated world is bound
	}
	t.mu.Lock()
	targetID := ""
	for id, frameID := range t.mainFrames {
		if frameID == p.Context.AuxData.FrameID {
			targetID = id
			break
		}
	}
	if targetID == "" {
		t.mu.Unlock()
		return
	}
	if _, dup := t.contexts[targetID]; dup {
		t.mu.Unlock()
		return
	}
	t.contexts[targetID] = p.Context.ID
	sid := t.sessions[targetID]
	t.mu.Unlock()
	if sid == "" {
		return
	}
	// Binding must be (re)issued inside the new context. The
	// CDP calls run off the event dispatcher so event order
	// for input is never delayed by them.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = t.client.Call(ctx, sid, "Runtime.addBinding", map[string]any{
			"name": capture.BindingName, "executionContextId": p.Context.ID,
		})
		_, _ = t.client.Call(ctx, sid, "Runtime.evaluate", map[string]any{
			"expression": capture.Script, "contextId": p.Context.ID,
		})
	}()
}

func (t *pageTracker) onFrameNavigated(ev cdp.Event) {
	var p struct {
		Frame struct {
			ID       string `json:"id"`
			LoaderID string `json:"loaderId"`
			URL      string `json:"url"`
		} `json:"frame"`
	}
	if err := json.Unmarshal(ev.Params, &p); err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for targetID, frameID := range t.mainFrames {
		if frameID == p.Frame.ID {
			t.urls[targetID] = p.Frame.URL
			return
		}
	}
}

func (t *pageTracker) onBindingCalled(ev cdp.Event) {
	var p struct {
		Name    string `json:"name"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(ev.Params, &p); err != nil || p.Name != capture.BindingName {
		return
	}
	var pageEv PageEvent
	if err := json.Unmarshal([]byte(p.Payload), &pageEv); err != nil {
		return
	}
	t.mu.Lock()
	targetID := ""
	for id, sid := range t.sessions {
		if sid == ev.SessionID {
			targetID = id
			break
		}
	}
	if targetID == "" {
		t.mu.Unlock()
		return
	}
	if pageEv.Kind == "visibility" {
		t.visible[targetID] = pageEv.Visible
	}
	t.mu.Unlock()
	pageEv.Source = targetID
	if t.onEvent != nil {
		t.onEvent(targetID, pageEv)
	}
}
