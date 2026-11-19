package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChromium is a scripted in-process CDP endpoint standing in
// for one browser instance: it serves /json/version discovery and
// a WebSocket that answers the command subset the sync feature
// uses, records every command it receives, and can push protocol
// events (binding calls, target lifecycle, navigations). After the
// observer script is evaluated on a session it reports that page's
// visibility, exactly as the real injected script does.
type fakeChromium struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	pages    map[string]*fakePage // targetID -> page
	sessions map[string]string    // sessionID -> targetID
	seq      int
	commands []fakeCommand
	conn     *websocket.Conn
	activeID string // targetID reported visible
	viewport map[string][2]float64

	// Attach scripting for the concurrency/readiness tests:
	// attachDelay slows every Target.attachToTarget answer,
	// failAttach makes attach fail for the named targets, and
	// attachCounts records how many attach commands arrived.
	attachDelay  time.Duration
	failAttach   map[string]bool
	attachCounts map[string]int
	lastCreated  string
}

type fakePage struct {
	targetID string
	url      string
}

type fakeCommand struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

type fakeWire struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *fakeWireError  `json:"error,omitempty"`
}

type fakeWireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newFakeChromium(t *testing.T, pageURLs map[string]string, activeID string) *fakeChromium {
	t.Helper()
	f := &fakeChromium{
		t:            t,
		pages:        make(map[string]*fakePage),
		sessions:     make(map[string]string),
		viewport:     make(map[string][2]float64),
		activeID:     activeID,
		failAttach:   make(map[string]bool),
		attachCounts: make(map[string]int),
	}
	for id, url := range pageURLs {
		f.pages[id] = &fakePage{targetID: id, url: url}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/devtools/browser/fake"
		_ = json.NewEncoder(w).Encode(map[string]string{"webSocketDebuggerUrl": wsURL})
	})
	mux.HandleFunc("/devtools/browser/fake", f.serveWS)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// Port returns the fake browser's HTTP port (its "CDP port").
func (f *fakeChromium) Port() int {
	var port int
	_, _ = fmt.Sscanf(strings.TrimPrefix(f.srv.URL, "http://127.0.0.1:"), "%d", &port)
	return port
}

func (f *fakeChromium) setViewport(targetID string, w, h float64) {
	f.mu.Lock()
	f.viewport[targetID] = [2]float64{w, h}
	f.mu.Unlock()
}

func (f *fakeChromium) serveWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.conn = conn
	f.mu.Unlock()
	ctx := context.Background()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg fakeWire
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID == 0 {
			continue
		}
		f.mu.Lock()
		f.commands = append(f.commands, fakeCommand{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params})
		f.mu.Unlock()
		result, errMsg := f.answer(msg)
		outWire := fakeWire{ID: msg.ID, Result: result}
		if errMsg != "" {
			outWire.Result = nil
			outWire.Error = &fakeWireError{Code: -32000, Message: errMsg}
		}
		out, _ := json.Marshal(outWire)
		if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
			return
		}
		// The injected script reports visibility right after it is
		// evaluated on a page.
		if msg.Method == "Runtime.evaluate" {
			f.reportVisibility(msg.SessionID)
		}
		// A real browser with target discovery on announces a
		// created target right after Target.createTarget answers.
		if msg.Method == "Target.createTarget" && errMsg == "" {
			f.mu.Lock()
			created := f.lastCreated
			url := ""
			if p := f.pages[created]; p != nil {
				url = p.url
			}
			f.mu.Unlock()
			if created != "" {
				f.pushTargetCreatedEvent(created, url)
			}
		}
	}
}

func (f *fakeChromium) answer(msg fakeWire) (json.RawMessage, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	params := map[string]any{}
	if len(msg.Params) > 0 {
		_ = json.Unmarshal(msg.Params, &params)
	}
	switch msg.Method {
	case "Target.getTargets":
		infos := []map[string]any{}
		for _, p := range f.pages {
			infos = append(infos, map[string]any{
				"targetId": p.targetID, "type": "page", "title": "页面", "url": p.url, "attached": false,
			})
		}
		b, _ := json.Marshal(map[string]any{"targetInfos": infos})
		return b, ""
	case "Target.attachToTarget":
		targetID, _ := params["targetId"].(string)
		f.attachCounts[targetID]++
		if f.failAttach[targetID] {
			return nil, "fake attach failure"
		}
		if f.attachDelay > 0 {
			time.Sleep(f.attachDelay)
		}
		sessionID := "sess-" + targetID
		f.sessions[sessionID] = targetID
		b, _ := json.Marshal(map[string]any{"sessionId": sessionID})
		return b, ""
	case "Page.getFrameTree":
		targetID := f.sessions[msg.SessionID]
		b, _ := json.Marshal(map[string]any{"frameTree": map[string]any{
			"frame": map[string]any{"id": "frame-" + targetID},
		}})
		return b, ""
	case "Page.createIsolatedWorld":
		return json.RawMessage(`{"executionContextId": 42}`), ""
	case "Page.getLayoutMetrics":
		targetID := f.sessions[msg.SessionID]
		vp := f.viewport[targetID]
		if vp[0] == 0 {
			vp = [2]float64{1280, 800}
		}
		b, _ := json.Marshal(map[string]any{"cssVisualViewport": map[string]any{
			"clientWidth": vp[0], "clientHeight": vp[1],
		}})
		return b, ""
	case "Target.createTarget":
		f.seq++
		id := fmt.Sprintf("created-%d", f.seq)
		url, _ := params["url"].(string)
		f.pages[id] = &fakePage{targetID: id, url: url}
		f.lastCreated = id
		b, _ := json.Marshal(map[string]any{"targetId": id})
		return b, ""
	case "Target.closeTarget":
		targetID, _ := params["targetId"].(string)
		delete(f.pages, targetID)
		return json.RawMessage(`{"success": true}`), ""
	}
	return json.RawMessage(`{}`), ""
}

// setAttachDelay slows every subsequent attach answer.
func (f *fakeChromium) setAttachDelay(d time.Duration) {
	f.mu.Lock()
	f.attachDelay = d
	f.mu.Unlock()
}

// failAttachFor makes Target.attachToTarget fail for a target.
func (f *fakeChromium) failAttachFor(targetID string) {
	f.mu.Lock()
	f.failAttach[targetID] = true
	f.mu.Unlock()
}

// attachCount reports how many attach commands arrived for a
// target (the single-flight attach proof).
func (f *fakeChromium) attachCount(targetID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attachCounts[targetID]
}

// reportVisibility pushes the observer script's visibility report
// for the page behind one session.
func (f *fakeChromium) reportVisibility(sessionID string) {
	f.mu.Lock()
	targetID := f.sessions[sessionID]
	page := f.pages[targetID]
	conn := f.conn
	active := f.activeID
	f.mu.Unlock()
	if conn == nil || page == nil {
		return
	}
	state := "hidden"
	if targetID == active {
		state = "visible"
	}
	payload, _ := json.Marshal(map[string]any{
		"kind": "visibility", "state": state, "href": page.url,
	})
	ev, _ := json.Marshal(fakeWire{
		Method:    "Runtime.bindingCalled",
		SessionID: sessionID,
		Params:    mustJSON(map[string]any{"name": syncBindingName, "payload": string(payload), "executionContextId": 42}),
	})
	_ = conn.Write(context.Background(), websocket.MessageText, ev)
}

// PushBindingEvent injects one observer-script event on a page.
func (f *fakeChromium) PushBindingEvent(targetID string, ev PageEvent) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatalf("fake browser not connected")
	}
	payload, _ := json.Marshal(ev)
	msg, _ := json.Marshal(fakeWire{
		Method:    "Runtime.bindingCalled",
		SessionID: "sess-" + targetID,
		Params:    mustJSON(map[string]any{"name": syncBindingName, "payload": string(payload), "executionContextId": 42}),
	})
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		f.t.Fatalf("push event: %v", err)
	}
}

// PushTargetCreated injects a Target.targetCreated event.
func (f *fakeChromium) PushTargetCreated(targetID, url string) {
	f.mu.Lock()
	f.pages[targetID] = &fakePage{targetID: targetID, url: url}
	f.mu.Unlock()
	f.pushTargetCreatedEvent(targetID, url)
}

func (f *fakeChromium) pushTargetCreatedEvent(targetID, url string) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		return
	}
	msg, _ := json.Marshal(fakeWire{
		Method: "Target.targetCreated",
		Params: mustJSON(map[string]any{"targetInfo": map[string]any{
			"targetId": targetID, "type": "page", "title": "新标签", "url": url,
		}}),
	})
	_ = conn.Write(context.Background(), websocket.MessageText, msg)
}

// PushExecutionContextCreated injects a
// Runtime.executionContextCreated event for the sync isolated
// world on the given page session, with the supplied frame ID —
// used to prove subframe contexts are never bound.
func (f *fakeChromium) PushExecutionContextCreated(targetID, frameID string, contextID int) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatalf("fake browser not connected")
	}
	msg, _ := json.Marshal(fakeWire{
		Method:    "Runtime.executionContextCreated",
		SessionID: "sess-" + targetID,
		Params: mustJSON(map[string]any{"context": map[string]any{
			"id": contextID, "name": syncWorldName,
			"auxData": map[string]any{"frameId": frameID, "isDefault": false},
		}}),
	})
	if err := conn.Write(context.Background(), websocket.MessageText, msg); err != nil {
		f.t.Fatalf("push context event: %v", err)
	}
}

// PushTargetDestroyed injects a Target.targetDestroyed event.
func (f *fakeChromium) PushTargetDestroyed(targetID string) {
	f.mu.Lock()
	delete(f.pages, targetID)
	conn := f.conn
	f.mu.Unlock()
	msg, _ := json.Marshal(fakeWire{
		Method: "Target.targetDestroyed",
		Params: mustJSON(map[string]any{"targetId": targetID}),
	})
	_ = conn.Write(context.Background(), websocket.MessageText, msg)
}

// PushFrameNavigated injects a main-frame Page.frameNavigated
// event for a page, as a committed navigation (or, with the same
// URL and a new loader ID, a reload).
func (f *fakeChromium) PushFrameNavigated(targetID, url, loaderID string) {
	f.mu.Lock()
	conn := f.conn
	if page := f.pages[targetID]; page != nil {
		page.url = url
	}
	f.mu.Unlock()
	msg, _ := json.Marshal(fakeWire{
		Method:    "Page.frameNavigated",
		SessionID: "sess-" + targetID,
		Params: mustJSON(map[string]any{"frame": map[string]any{
			"id": "frame-" + targetID, "loaderId": loaderID, "url": url,
		}}),
	})
	_ = conn.Write(context.Background(), websocket.MessageText, msg)
}

// Commands returns the recorded commands, optionally filtered by
// method.
func (f *fakeChromium) Commands(method string) []fakeCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakeCommand{}
	for _, c := range f.commands {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// WaitCommands polls until at least n commands of a method were
// recorded or the deadline passes.
func (f *fakeChromium) WaitCommands(method string, n int, timeout time.Duration) []fakeCommand {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := f.Commands(method); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	return f.Commands(method)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
