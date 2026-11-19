package service

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// syncCaptureScript is the page observer installed on synced
// browsers' pages (see the file's own header for its contract).
//
//go:embed sync_capture_script.js
var syncCaptureScript string

const (
	syncWorldName   = "debrowser-sync"
	syncBindingName = "__debrowserSyncReport"
)

// PageView is the externally visible state of one tracked page
// target: whether it currently reports itself visible (the active
// tab signal) and its last known URL (session memory only).
type PageView struct {
	Visible bool
	URL     string
}

// pageCallbacks receives tracker observations. Every callback is
// invoked from the CDP read path and must never block: handlers
// record state and enqueue work only.
type pageCallbacks struct {
	OnPageEvent       func(targetID string, ev PageEvent)
	OnVisibility      func(targetID string)
	OnNavigate        func(targetID, url string, reload bool)
	OnTargetCreated   func(targetID string)
	OnTargetDestroyed func(targetID string)
}

type trackedPage struct {
	targetID  string
	sessionID string
	frameID   string
	visible   bool
	url       string
	lastNav   string
	lastLoad  string
	// ready is set once the page's session exists, the observer
	// is installed and the tracker can route events for it. A
	// target ID returned by Target.createTarget is NOT usable
	// until the corresponding page is ready.
	ready bool
}

// attachCall is the single-flight handle for one target's attach:
// concurrent attach attempts for the same target (the initial
// GetTargets sweep racing Target.targetCreated, or duplicate
// events) all wait on the same call and share its result.
type attachCall struct {
	done chan struct{}
	err  error
}

// pageTracker owns one browser connection's page bookkeeping: it
// attaches to every page target, installs the observer script in
// an isolated world, scopes the reporting binding to that world
// (ordinary page scripts never see it), and turns protocol events
// into typed callbacks. Target identity comes only from CDP
// session and target IDs — never from titles, URLs or list order.
type pageTracker struct {
	client *cdp.Client
	cb     pageCallbacks

	mu         sync.Mutex
	pages      map[string]*trackedPage // targetID -> page
	bySession  map[string]string       // sessionID -> targetID
	contexts   map[int]string          // executionContextID -> sessionID
	attaching  map[string]*attachCall  // targetID -> in-flight attach
	mainFrames map[string]string       // sessionID -> main frame ID
}

func newPageTracker(client *cdp.Client, cb pageCallbacks) *pageTracker {
	return &pageTracker{
		client:     client,
		cb:         cb,
		pages:      make(map[string]*trackedPage),
		bySession:  make(map[string]string),
		contexts:   make(map[int]string),
		attaching:  make(map[string]*attachCall),
		mainFrames: make(map[string]string),
	}
}

// Start enables target discovery and attaches to all current page
// targets.
func (t *pageTracker) Start(ctx context.Context) error {
	if err := t.client.SetDiscoverTargets(ctx, true); err != nil {
		return err
	}
	infos, err := t.client.GetTargets(ctx)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.Type != "page" {
			continue
		}
		if err := t.attach(ctx, info.TargetID); err != nil {
			return fmt.Errorf("接入页面失败：%w", err)
		}
	}
	return nil
}

// attach connects one page target: enables Runtime and Page,
// registers the observer for future documents, and installs it
// into the current document's isolated world. Attach is
// single-flight per target: concurrent attempts join the
// in-flight call and share its outcome instead of attaching
// twice. A failed attach leaves no page, session or context
// residue behind, and its error is reported to every waiter —
// callers must not treat a failed attach as a created page.
func (t *pageTracker) attach(ctx context.Context, targetID string) error {
	t.mu.Lock()
	if page, ok := t.pages[targetID]; ok && page.ready {
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

	err := t.attachOnce(ctx, targetID)

	t.mu.Lock()
	delete(t.attaching, targetID)
	call.err = err
	close(call.done)
	t.mu.Unlock()
	return err
}

// attachOnce performs the actual attach sequence for one target.
func (t *pageTracker) attachOnce(ctx context.Context, targetID string) error {
	sessionID, err := t.client.AttachToTarget(ctx, targetID)
	if err != nil {
		return err
	}
	page := &trackedPage{targetID: targetID, sessionID: sessionID}
	t.mu.Lock()
	t.pages[targetID] = page
	t.bySession[sessionID] = targetID
	t.mu.Unlock()
	// fail removes every trace of the half-attached page so a
	// later retry starts clean and nothing stale can route input.
	fail := func(err error) error {
		t.mu.Lock()
		delete(t.pages, targetID)
		delete(t.bySession, sessionID)
		delete(t.mainFrames, sessionID)
		for id, sess := range t.contexts {
			if sess == sessionID {
				delete(t.contexts, id)
			}
		}
		t.mu.Unlock()
		return err
	}

	if _, err := t.client.SessionCall(ctx, sessionID, "Runtime.enable", nil); err != nil {
		return fail(err)
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Page.enable", nil); err != nil {
		return fail(err)
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": syncCaptureScript, "worldName": syncWorldName}); err != nil {
		return fail(err)
	}
	// Current document: create the isolated world, scope the
	// binding to it, then evaluate the observer inside it. The
	// isolated world is created for the MAIN frame only — the
	// observer is a top-level page mechanism; subframe contexts
	// are never bound (see executionContextCreated handling).
	frameID, err := t.mainFrameID(ctx, sessionID)
	if err != nil {
		return fail(err)
	}
	t.mu.Lock()
	page.frameID = frameID
	t.mainFrames[sessionID] = frameID
	t.mu.Unlock()
	raw, err := t.client.SessionCall(ctx, sessionID, "Page.createIsolatedWorld",
		map[string]any{"frameId": frameID, "worldName": syncWorldName, "grantUniversalAccess": false})
	if err != nil {
		return fail(err)
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := json.Unmarshal(raw, &world); err != nil || world.ExecutionContextID == 0 {
		return fail(fmt.Errorf("创建页面隔离环境失败"))
	}
	t.mu.Lock()
	t.contexts[world.ExecutionContextID] = sessionID
	t.mu.Unlock()
	if err := t.addBinding(ctx, sessionID, world.ExecutionContextID); err != nil {
		return fail(err)
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Runtime.evaluate",
		map[string]any{"expression": syncCaptureScript, "contextId": world.ExecutionContextID}); err != nil {
		return fail(err)
	}
	t.mu.Lock()
	page.ready = true
	t.mu.Unlock()
	return nil
}

// WaitReady blocks until the target's page is fully attached
// (session established, observer installed, tracking live) or
// the context ends. A Target.createTarget result alone never
// proves a page can receive input — callers that build a mapping
// from a creation result must pass through here first.
func (t *pageTracker) WaitReady(ctx context.Context, targetID string) error {
	for {
		t.mu.Lock()
		page, ok := t.pages[targetID]
		ready := ok && page.ready
		_, attaching := t.attaching[targetID]
		t.mu.Unlock()
		if ready {
			return nil
		}
		if !attaching {
			// No attach in flight: kick one off and wait for it.
			if err := t.attach(ctx, targetID); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (t *pageTracker) addBinding(ctx context.Context, sessionID string, contextID int) error {
	_, err := t.client.SessionCall(ctx, sessionID, "Runtime.addBinding",
		map[string]any{"name": syncBindingName, "executionContextId": contextID})
	return err
}

func (t *pageTracker) mainFrameID(ctx context.Context, sessionID string) (string, error) {
	raw, err := t.client.SessionCall(ctx, sessionID, "Page.getFrameTree", nil)
	if err != nil {
		return "", err
	}
	var tree struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil || tree.FrameTree.Frame.ID == "" {
		return "", fmt.Errorf("读取页面结构失败")
	}
	return tree.FrameTree.Frame.ID, nil
}

// HandleEvent consumes one protocol event from the connection.
// It never issues protocol commands synchronously (that would
// deadlock the read loop); reactions that need commands run in
// their own goroutine.
func (t *pageTracker) HandleEvent(ev cdp.Event) {
	switch ev.Method {
	case "Target.targetCreated":
		var p targetCreatedParams
		if json.Unmarshal(ev.Params, &p) != nil || p.TargetInfo.Type != "page" {
			return
		}
		targetID := p.TargetInfo.TargetID
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := t.attach(ctx, targetID); err != nil {
				// A failed attach is NOT a created page: the
				// OnTargetCreated callback must not fire, so no
				// mapping is ever built on a page that cannot
				// receive input.
				return
			}
			if t.cb.OnTargetCreated != nil {
				t.cb.OnTargetCreated(targetID)
			}
		}()
	case "Target.targetDestroyed":
		var p targetDestroyedParams
		if json.Unmarshal(ev.Params, &p) != nil {
			return
		}
		t.mu.Lock()
		if page, ok := t.pages[p.TargetID]; ok {
			delete(t.bySession, page.sessionID)
			delete(t.mainFrames, page.sessionID)
			for id, sess := range t.contexts {
				if sess == page.sessionID {
					delete(t.contexts, id)
				}
			}
			delete(t.pages, p.TargetID)
		}
		t.mu.Unlock()
		if t.cb.OnTargetDestroyed != nil {
			t.cb.OnTargetDestroyed(p.TargetID)
		}
	case "Runtime.executionContextCreated":
		var p executionContextCreatedParams
		if json.Unmarshal(ev.Params, &p) != nil || p.Context.Name != syncWorldName {
			return
		}
		sessionID := ev.SessionID
		// Only the MAIN frame's isolated world may carry the
		// reporting binding: a subframe context (created by the
		// per-frame new-document script) must never be treated as
		// the top-level page. When the session's main frame is
		// not known yet, the context is left unbound — attach()
		// binds the main-frame context explicitly.
		t.mu.Lock()
		mainFrame, known := t.mainFrames[sessionID]
		t.mu.Unlock()
		if !known || p.Context.AuxData.FrameID != mainFrame {
			return
		}
		t.mu.Lock()
		t.contexts[p.Context.ID] = sessionID
		t.mu.Unlock()
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.addBinding(ctx, sessionID, p.Context.ID)
		}()
	case "Runtime.bindingCalled":
		var p bindingCalledParams
		if json.Unmarshal(ev.Params, &p) != nil || p.Name != syncBindingName {
			return
		}
		pageEvent, ok := parsePageEvent(p.Payload)
		if !ok {
			return
		}
		t.mu.Lock()
		targetID := t.bySession[ev.SessionID]
		page := t.pages[targetID]
		t.mu.Unlock()
		if page == nil {
			return
		}
		if pageEvent.Kind == "visibility" {
			t.mu.Lock()
			page.visible = pageEvent.State == "visible"
			if pageEvent.Href != "" {
				page.url = pageEvent.Href
			}
			t.mu.Unlock()
			if t.cb.OnVisibility != nil {
				t.cb.OnVisibility(targetID)
			}
			return
		}
		if t.cb.OnPageEvent != nil {
			t.cb.OnPageEvent(targetID, pageEvent)
		}
	case "Page.frameNavigated":
		var p frameNavigatedParams
		if json.Unmarshal(ev.Params, &p) != nil || p.Frame.ParentID != "" {
			return
		}
		t.mu.Lock()
		targetID := t.bySession[ev.SessionID]
		page := t.pages[targetID]
		if page == nil {
			t.mu.Unlock()
			return
		}
		reload := p.Frame.URL == page.lastNav && page.lastNav != "" && p.Frame.LoaderID != page.lastLoad
		page.lastNav = p.Frame.URL
		page.lastLoad = p.Frame.LoaderID
		page.url = p.Frame.URL
		// A frame without a parent ID is the main frame; keep the
		// session's main-frame identity current for the isolated
		// world binding check.
		t.mainFrames[ev.SessionID] = p.Frame.ID
		t.mu.Unlock()
		if t.cb.OnNavigate != nil {
			t.cb.OnNavigate(targetID, p.Frame.URL, reload)
		}
	}
}

// SessionFor returns the CDP session ID of a tracked page target.
func (t *pageTracker) SessionFor(targetID string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	page, ok := t.pages[targetID]
	if !ok {
		return "", false
	}
	return page.sessionID, true
}

// Pages snapshots every tracked page's visibility and URL.
func (t *pageTracker) Pages() map[string]PageView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]PageView, len(t.pages))
	for id, page := range t.pages {
		out[id] = PageView{Visible: page.visible, URL: page.url}
	}
	return out
}

// ActiveTarget returns the instance's active page target: the
// unique page target currently reporting visible. ok is false
// when zero or more than one page reports visible — the state is
// then genuinely ambiguous (for example two visible top-level
// windows) and must not be guessed.
func (t *pageTracker) ActiveTarget() (targetID string, view PageView, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	found := ""
	for id, page := range t.pages {
		if !page.visible {
			continue
		}
		if found != "" {
			return "", PageView{}, false
		}
		found = id
	}
	if found == "" {
		return "", PageView{}, false
	}
	page := t.pages[found]
	return found, PageView{Visible: true, URL: page.url}, true
}

// Viewport returns the page's CSS viewport size in pixels.
func (t *pageTracker) Viewport(ctx context.Context, targetID string) (width, height float64, err error) {
	sessionID, ok := t.SessionFor(targetID)
	if !ok {
		return 0, 0, fmt.Errorf("页面未接入")
	}
	raw, err := t.client.SessionCall(ctx, sessionID, "Page.getLayoutMetrics", nil)
	if err != nil {
		return 0, 0, err
	}
	var metrics struct {
		CSSVisualViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
		LayoutViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"layoutViewport"`
	}
	if err := json.Unmarshal(raw, &metrics); err != nil {
		return 0, 0, fmt.Errorf("读取页面尺寸失败：%w", err)
	}
	if metrics.CSSVisualViewport.ClientWidth > 0 {
		return metrics.CSSVisualViewport.ClientWidth, metrics.CSSVisualViewport.ClientHeight, nil
	}
	if metrics.LayoutViewport.ClientWidth > 0 {
		return metrics.LayoutViewport.ClientWidth, metrics.LayoutViewport.ClientHeight, nil
	}
	return 0, 0, fmt.Errorf("页面尺寸不可用")
}
