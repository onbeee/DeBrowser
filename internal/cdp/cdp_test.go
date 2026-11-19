package cdp

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestValidateLoopbackWSURL(t *testing.T) {
	good := []string{"ws://127.0.0.1:9222/devtools/browser/x", "ws://[::1]:9222/x"}
	for _, u := range good {
		if err := ValidateLoopbackWSURL(u, 9222); err != nil {
			t.Fatalf("%s: %v", u, err)
		}
	}
	bad := []string{
		"ws://192.168.1.5:9222/x",  // remote host
		"ws://example.com:9222/x",  // hostname
		"http://127.0.0.1:9222/x",  // wrong scheme
		"ws://127.0.0.1:9333/x",    // wrong port (want 9222)
		"ws://localhost:9222/x",    // hostname form is not an IP literal
		"ws://127.0.0.1.evil.com/", // suffix trick
	}
	for _, u := range bad {
		if err := ValidateLoopbackWSURL(u, 9222); err == nil {
			t.Fatalf("%s accepted, want rejection", u)
		}
	}
}

// fakeCDP is an in-process CDP-shaped WebSocket server: it
// answers Target.createTarget and one slow method, emits one
// event on connect, and can hang up on demand.
type fakeCDP struct {
	srv   *httptest.Server
	port  int
	connC chan *websocket.Conn
}

// killConn drops the live WebSocket connection server-side,
// simulating the browser going away mid-call.
func (f *fakeCDP) killConn() {
	select {
	case conn := <-f.connC:
		_ = conn.CloseNow()
	default:
	}
}

func newFakeCDP(t *testing.T) *fakeCDP {
	t.Helper()
	f := &fakeCDP{connC: make(chan *websocket.Conn, 1)}
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
		f.connC <- conn
		// One unsolicited event, as real browsers send.
		_ = wsjson.Write(r.Context(), conn, map[string]any{
			"method": "Target.targetCreated",
			"params": map[string]any{"targetInfo": map[string]any{"targetId": "T0"}},
		})
		for {
			var msg struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := wsjson.Read(r.Context(), conn, &msg); err != nil {
				return
			}
			switch msg.Method {
			case "Target.createTarget":
				_ = wsjson.Write(r.Context(), conn, map[string]any{
					"id": msg.ID, "result": map[string]any{"targetId": "T-NEW"},
				})
			case "Slow.method":
				time.Sleep(2 * time.Second)
				_ = wsjson.Write(r.Context(), conn, map[string]any{"id": msg.ID, "result": map[string]any{}})
			default:
				_ = wsjson.Write(r.Context(), conn, map[string]any{"id": msg.ID, "result": map[string]any{"ok": true}})
			}
		}
	})
	f.srv = httptest.NewServer(mux)
	_, portStr, _ := net.SplitHostPort(f.srv.Listener.Addr().String())
	_ = json.Unmarshal([]byte(portStr), &f.port)
	var p int
	for _, ch := range portStr {
		p = p*10 + int(ch-'0')
	}
	f.port = p
	t.Cleanup(f.srv.Close)
	return f
}

func TestDiscoverDialCallAndEvents(t *testing.T) {
	f := newFakeCDP(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wsURL, err := Discover(ctx, f.port)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	client, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	gotEvent := make(chan string, 1)
	client.On("Target.targetCreated", func(ev Event) {
		var p struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
			} `json:"targetInfo"`
		}
		_ = json.Unmarshal(ev.Params, &p)
		gotEvent <- p.TargetInfo.TargetID
	})

	id, err := client.CallString(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, "targetId")
	if err != nil || id != "T-NEW" {
		t.Fatalf("createTarget = %q, %v", id, err)
	}
	select {
	case tid := <-gotEvent:
		if tid != "T0" {
			t.Fatalf("event target = %q", tid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("event not delivered")
	}
}

func TestCallTimeoutAndPendingCleanup(t *testing.T) {
	f := newFakeCDP(t)
	ctx := context.Background()
	wsURL, err := Discover(ctx, f.port)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	short, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := client.Call(short, "", "Slow.method", nil); err == nil {
		t.Fatal("slow call should time out")
	}
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending after timeout = %d, want 0", pending)
	}
}

func TestDisconnectFailsPendingAndClosesDone(t *testing.T) {
	f := newFakeCDP(t)
	ctx := context.Background()
	wsURL, err := Discover(ctx, f.port)
	if err != nil {
		t.Fatal(err)
	}
	client, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Call(ctx, "", "Slow.method", nil)
		errCh <- err
	}()
	time.Sleep(100 * time.Millisecond)
	f.killConn() // browser goes away mid-call
	select {
	case err := <-errCh:
		if err == nil || !strings.Contains(err.Error(), "关闭") {
			t.Fatalf("pending call error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call not failed on disconnect")
	}
	select {
	case <-client.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("Done not closed")
	}
	if _, err := client.Call(ctx, "", "Any.method", nil); err == nil {
		t.Fatal("call on closed client should fail")
	}
}

func TestDiscoverRejectsWrongPortEndpoint(t *testing.T) {
	// A version endpoint that hands back a URL on a DIFFERENT
	// port than the one assigned must be rejected.
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"webSocketDebuggerUrl": "ws://127.0.0.1:1/devtools/browser/x",
		})
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()
	_, portStr, _ := net.SplitHostPort(srv.Listener.Addr().String())
	p := 0
	for _, ch := range portStr {
		p = p*10 + int(ch-'0')
	}
	if _, err := Discover(context.Background(), p); err == nil {
		t.Fatal("endpoint on a different port accepted")
	}
}
