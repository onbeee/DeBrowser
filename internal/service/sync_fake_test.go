package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// fakeBrowserCDP is an in-process CDP-shaped server for sync
// tests: it answers the handful of commands the tracker and
// workers use, counts Target.createTarget calls, and can
// delay or fail creation on demand.
type fakeBrowserCDP struct {
	srv         *httptest.Server
	port        int
	creates     atomic.Int64
	createdMu   sync.Mutex
	createdURLs []string
	closes      atomic.Int64
	detaches    atomic.Int64
	delay       time.Duration
	fail        bool
	// failEnable makes Page.enable fail (attach cleanup tests).
	failEnable bool

	callsMu sync.Mutex
	calls   []recordedCall
}

// recordedCall is one input command the fake received.
type recordedCall struct {
	Method    string
	SessionID string
	Type      string // keyDown/keyUp/mousePressed/mouseReleased
	Code      string
	Button    string
}

func (f *fakeBrowserCDP) record(method, sessionID string, params json.RawMessage) {
	var p struct {
		Type   string `json:"type"`
		Code   string `json:"code"`
		Button string `json:"button"`
	}
	_ = json.Unmarshal(params, &p)
	f.callsMu.Lock()
	f.calls = append(f.calls, recordedCall{Method: method, SessionID: sessionID, Type: p.Type, Code: p.Code, Button: p.Button})
	f.callsMu.Unlock()
}

func (f *fakeBrowserCDP) createdURL(i int) string {
	f.createdMu.Lock()
	defer f.createdMu.Unlock()
	if i < 0 || i >= len(f.createdURLs) {
		return "<none>"
	}
	return f.createdURLs[i]
}

func (f *fakeBrowserCDP) findCalls(method, sessionID, typ, code string) int {
	f.callsMu.Lock()
	defer f.callsMu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method && c.SessionID == sessionID && c.Type == typ && (code == "" || c.Code == code) {
			n++
		}
	}
	return n
}

func newFakeBrowserCDP(t *testing.T) *fakeBrowserCDP {
	t.Helper()
	f := &fakeBrowserCDP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://" + f.srv.Listener.Addr().String() + "/devtools/browser/fake",
		})
	})
	mux.HandleFunc("/devtools/browser/fake", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for {
			var msg struct {
				ID        int64           `json:"id"`
				Method    string          `json:"method"`
				SessionID string          `json:"sessionId"`
				Params    json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			reply := func(result any) {
				_ = wsjson.Write(r.Context(), conn, map[string]any{"id": msg.ID, "result": result})
			}
			switch msg.Method {
			case "Input.dispatchKeyEvent", "Input.dispatchMouseEvent":
				f.record(msg.Method, msg.SessionID, msg.Params)
				reply(map[string]any{})
			case "Target.closeTarget":
				f.closes.Add(1)
				reply(map[string]any{"success": true})
			case "Target.detachFromTarget":
				f.detaches.Add(1)
				reply(map[string]any{})
			case "Page.enable":
				if f.failEnable {
					_ = wsjson.Write(r.Context(), conn, map[string]any{
						"id": msg.ID, "error": map[string]any{"code": -1, "message": "enable failed"},
					})
					continue
				}
				reply(map[string]any{})
			case "Target.createTarget":
				n := f.creates.Add(1)
				var cp struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(msg.Params, &cp)
				f.createdMu.Lock()
				f.createdURLs = append(f.createdURLs, cp.URL)
				f.createdMu.Unlock()
				if f.delay > 0 {
					time.Sleep(f.delay)
				}
				if f.fail {
					_ = wsjson.Write(r.Context(), conn, map[string]any{
						"id": msg.ID, "error": map[string]any{"code": -1, "message": "cannot create"},
					})
					continue
				}
				reply(map[string]any{"targetId": fmt.Sprintf("FT-%d", n)})
			case "Target.attachToTarget":
				var p struct {
					TargetID string `json:"targetId"`
				}
				_ = json.Unmarshal(msg.Params, &p)
				reply(map[string]any{"sessionId": "S-" + p.TargetID})
			case "Page.getFrameTree":
				reply(map[string]any{"frameTree": map[string]any{
					"frame": map[string]any{"id": "F-" + msg.SessionID},
				}})
			case "Page.getLayoutMetrics":
				reply(map[string]any{"layoutViewport": map[string]any{"clientWidth": 1000, "clientHeight": 800}})
			case "Target.getTargets":
				reply(map[string]any{"targetInfos": []any{}})
			default:
				reply(map[string]any{})
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	_, portStr, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	p := 0
	for _, ch := range portStr {
		p = p*10 + int(ch-'0')
	}
	f.port = p
	t.Cleanup(f.srv.Close)
	return f
}

func dialFake(t *testing.T, f *fakeBrowserCDP) *cdp.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info := SyncRuntimeInfo{CDPPort: f.port}
	client, err := dialInstance(ctx, info)
	if err != nil {
		t.Fatalf("dial fake: %v", err)
	}
	t.Cleanup(client.Close)
	return client
}
