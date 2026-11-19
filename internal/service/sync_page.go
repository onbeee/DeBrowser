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

	mu        sync.Mutex
	pages     map[string]*trackedPage // targetID -> page
	bySession map[string]string       // sessionID -> targetID
	contexts  map[int]string          // executionContextID -> sessionID
}

func newPageTracker(client *cdp.Client, cb pageCallbacks) *pageTracker {
	return &pageTracker{
		client:    client,
		cb:        cb,
		pages:     make(map[string]*trackedPage),
		bySession: make(map[string]string),
		contexts:  make(map[int]string),
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
// into the current document's isolated world.
func (t *pageTracker) attach(ctx context.Context, targetID string) error {
	t.mu.Lock()
	_, exists := t.pages[targetID]
	t.mu.Unlock()
	if exists {
		return nil
	}
	sessionID, err := t.client.AttachToTarget(ctx, targetID)
	if err != nil {
		return err
	}
	page := &trackedPage{targetID: targetID, sessionID: sessionID}
	t.mu.Lock()
	t.pages[targetID] = page
	t.bySession[sessionID] = targetID
	t.mu.Unlock()

	if _, err := t.client.SessionCall(ctx, sessionID, "Runtime.enable", nil); err != nil {
		return err
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Page.enable", nil); err != nil {
		return err
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Page.addScriptToEvaluateOnNewDocument",
		map[string]any{"source": syncCaptureScript, "worldName": syncWorldName}); err != nil {
		return err
	}
	// Current document: create the isolated world, scope the
	// binding to it, then evaluate the observer inside it.
	frameID, err := t.mainFrameID(ctx, sessionID)
	if err != nil {
		return err
	}
	t.mu.Lock()
	page.frameID = frameID
	t.mu.Unlock()
	raw, err := t.client.SessionCall(ctx, sessionID, "Page.createIsolatedWorld",
		map[string]any{"frameId": frameID, "worldName": syncWorldName, "grantUniversalAccess": false})
	if err != nil {
		return err
	}
	var world struct {
		ExecutionContextID int `json:"executionContextId"`
	}
	if err := json.Unmarshal(raw, &world); err != nil || world.ExecutionContextID == 0 {
		return fmt.Errorf("创建页面隔离环境失败")
	}
	t.mu.Lock()
	t.contexts[world.ExecutionContextID] = sessionID
	t.mu.Unlock()
	if err := t.addBinding(ctx, sessionID, world.ExecutionContextID); err != nil {
		return err
	}
	if _, err := t.client.SessionCall(ctx, sessionID, "Runtime.evaluate",
		map[string]any{"expression": syncCaptureScript, "contextId": world.ExecutionContextID}); err != nil {
		return err
	}
	return nil
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
			_ = t.attach(ctx, targetID)
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
