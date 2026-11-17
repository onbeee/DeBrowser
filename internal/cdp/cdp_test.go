package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeCDPServer speaks just enough CDP for client tests: an HTTP
// /json/version endpoint and a WebSocket that answers commands
// with canned results and records what it received.
type fakeCDPServer struct {
	mu       sync.Mutex
	received []map[string]any
	wsURL    string
	srv      *httptest.Server
}

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	f := &fakeCDPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"product":              "Chrome/130.0.0.0",
			"webSocketDebuggerUrl": f.wsURL,
		})
	})
	mux.HandleFunc("/devtools/browser/fake", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			f.mu.Lock()
			f.received = append(f.received, msg)
			f.mu.Unlock()
			id := msg["id"]
			var result any
			switch msg["method"] {
			case "Browser.getVersion":
				result = map[string]any{"product": "Chrome/130.0.0.0"}
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "T1", "type": "page", "url": "https://a.example/"},
					{"targetId": "T2", "type": "page", "url": "https://b.example/"},
				}}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S-" + msg["params"].(map[string]any)["targetId"].(string)}
			case "Page.getLayoutMetrics":
				result = map[string]any{"layoutViewport": map[string]any{"clientWidth": 1280.0, "clientHeight": 720.0}}
			default:
				result = map[string]any{}
			}
			out, _ := json.Marshal(map[string]any{"id": id, "result": result})
			if err := conn.Write(r.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	f.wsURL = "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/devtools/browser/fake"
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCDPServer) port(t *testing.T) int {
	t.Helper()
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return port
}

func (f *fakeCDPServer) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []string{}
	for _, m := range f.received {
		out = append(out, fmt.Sprint(m["method"]))
	}
	return out
}

func TestClientAgainstFakeServer(t *testing.T) {
	f := newFakeCDPServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, f.port(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	prod, err := client.BrowserVersion(ctx)
	if err != nil || prod != "Chrome/130.0.0.0" {
		t.Fatalf("version = %q, %v", prod, err)
	}
	targets, err := client.Targets(ctx)
	if err != nil || len(targets) != 2 || targets[0].ID != "T1" {
		t.Fatalf("targets = %+v, %v", targets, err)
	}
	sid, err := client.Attach(ctx, "T2")
	if err != nil || sid != "S-T2" {
		t.Fatalf("attach = %q, %v", sid, err)
	}
	if err := client.MouseEvent(ctx, sid, "mousePressed", 10, 20, "left", 1, 1, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := client.KeyEvent(ctx, sid, "keyDown", 65, 0, "a", "a"); err != nil {
		t.Fatal(err)
	}
	if err := client.InsertText(ctx, sid, "中文"); err != nil {
		t.Fatal(err)
	}
	w, h, err := client.LayoutViewport(ctx, sid)
	if err != nil || w != 1280 || h != 720 {
		t.Fatalf("viewport = %v x %v, %v", w, h, err)
	}
	got := strings.Join(f.methods(), ",")
	for _, want := range []string{"Browser.getVersion", "Target.getTargets", "Target.attachToTarget",
		"Input.dispatchMouseEvent", "Input.dispatchKeyEvent", "Input.insertText", "Page.getLayoutMetrics"} {
		if !strings.Contains(got, want) {
			t.Fatalf("server never received %s (got %s)", want, got)
		}
	}
	// Input went to the attached session only.
	f.mu.Lock()
	for _, m := range f.received {
		if strings.HasPrefix(fmt.Sprint(m["method"]), "Input.") && m["sessionId"] != "S-T2" {
			t.Fatalf("input sent on wrong session: %v", m)
		}
	}
	f.mu.Unlock()
}

func TestFetchVersionRejectsNonCDP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := FetchVersion(ctx, port); err == nil {
		t.Fatal("a non-CDP HTTP service must not pass the version check")
	}
	if _, err := Dial(ctx, port); err == nil {
		t.Fatal("dial to a non-CDP service must fail")
	}
}

// TestEventHandlerCanCallSameClient reproduces the Stage 10
// review deadlock: the server pushes a target-destroyed event,
// and the handler reacts by issuing commands on the SAME client
// (exactly what re-attaching to a remaining page does). If event
// delivery ran inside the read loop, those commands could never
// be answered and would die on their context timeout.
func TestEventHandlerCanCallSameClient(t *testing.T) {
	f := newFakeCDPServer(t)
	// Extend the fake server: it already answers commands; here
	// we need it to push an event. The base fake has no push
	// hook, so this test uses its own server below.
	_ = f
	srv := newEventPushServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(ctx, srv.port(t))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	done := make(chan error, 1)
	client.SetEventHandler(func(ev Event) {
		if ev.Method != "Target.targetDestroyed" {
			return
		}
		callCtx, callCancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer callCancel()
		targets, err := client.Targets(callCtx)
		if err != nil {
			done <- err
			return
		}
		if len(targets) == 0 {
			done <- fmt.Errorf("no targets left to re-attach")
			return
		}
		_, err = client.Attach(callCtx, targets[0].ID)
		done <- err
	})
	// Ask the server (via a real command round-trip) to push the
	// event, then wait for the handler's re-attach to complete.
	if _, err := client.Call(ctx, "", "Test.pushDestroyed", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler re-attach failed (deadlock?): %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("handler never completed: event delivery is blocked")
	}
}

// eventPushServer is a fake CDP server that pushes a
// Target.targetDestroyed event when it receives the
// "Test.pushDestroyed" command (event first, then the response).
type eventPushServer struct {
	*fakeCDPServer
}

func newEventPushServer(t *testing.T) *eventPushServer {
	t.Helper()
	f := &fakeCDPServer{}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"product":              "Chrome/130.0.0.0",
			"webSocketDebuggerUrl": f.wsURL,
		})
	})
	mux.HandleFunc("/devtools/browser/fake", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			if msg["method"] == "Test.pushDestroyed" {
				ev, _ := json.Marshal(map[string]any{
					"method": "Target.targetDestroyed",
					"params": map[string]any{"targetId": "T9"},
				})
				if err := conn.Write(r.Context(), websocket.MessageText, ev); err != nil {
					return
				}
			}
			var result any
			switch msg["method"] {
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "T1", "type": "page", "url": "https://a.example/"},
				}}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S-T1"}
			default:
				result = map[string]any{}
			}
			out, _ := json.Marshal(map[string]any{"id": msg["id"], "result": result})
			if err := conn.Write(r.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	f.wsURL = "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/devtools/browser/fake"
	t.Cleanup(f.srv.Close)
	return &eventPushServer{f}
}
