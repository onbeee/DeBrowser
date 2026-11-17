package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/onbeee/DeBrowser/internal/cdp"
	"github.com/onbeee/DeBrowser/internal/wininput"
)

// recordingCDPServer is a fake CDP endpoint that records every
// command it receives, so tests can assert exactly which input
// events a follower was sent.
type recordingCDPServer struct {
	mu       sync.Mutex
	received []map[string]any
}

func (r *recordingCDPServer) start(t *testing.T) int {
	t.Helper()
	mux := http.NewServeMux()
	var wsURL string
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"product": "Chrome/130.0.0.0", "webSocketDebuggerUrl": wsURL,
		})
	})
	mux.HandleFunc("/devtools/browser/fake", func(w http.ResponseWriter, req *http.Request) {
		conn, err := websocket.Accept(w, req, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.CloseNow() }()
		for {
			_, data, err := conn.Read(req.Context())
			if err != nil {
				return
			}
			var msg map[string]any
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			r.mu.Lock()
			r.received = append(r.received, msg)
			r.mu.Unlock()
			var result any
			switch msg["method"] {
			case "Browser.getVersion":
				result = map[string]any{"product": "Chrome/130.0.0.0"}
			case "Target.getTargets":
				result = map[string]any{"targetInfos": []map[string]any{
					{"targetId": "T1", "type": "page", "url": "https://example.com/"},
				}}
			case "Target.attachToTarget":
				result = map[string]any{"sessionId": "S1"}
			case "Page.getLayoutMetrics":
				result = map[string]any{"layoutViewport": map[string]any{"clientWidth": 1024.0, "clientHeight": 640.0}}
			default:
				result = map[string]any{}
			}
			out, _ := json.Marshal(map[string]any{"id": msg["id"], "result": result})
			if err := conn.Write(req.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	wsURL = fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/fake", port)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return port
}

// sawInput reports whether a recorded command is the given CDP
// input dispatch with the given type parameter.
func (r *recordingCDPServer) sawInput(method, typ string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.received {
		if m["method"] != method {
			continue
		}
		params, _ := m["params"].(map[string]any)
		if params != nil && params["type"] == typ {
			return true
		}
	}
	return false
}

func (r *recordingCDPServer) clear() {
	r.mu.Lock()
	r.received = nil
	r.mu.Unlock()
}

// releaseTestSession builds a SyncService whose master and one
// follower are backed by recording CDP servers, in the active
// state, without starting the (platform-dependent) capture.
func releaseTestSession(t *testing.T) (*SyncService, *recordingCDPServer, *syncPeer, *syncPeer) {
	t.Helper()
	fl := newFakeLauncher()
	isvc, _, _ := testRuntimeService(t, fl)
	ssvc := NewSyncService(isvc, nil)

	masterRec := &recordingCDPServer{}
	followerRec := &recordingCDPServer{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mkPeer := func(id string, rec *recordingCDPServer) *syncPeer {
		client, err := cdp.Dial(ctx, rec.start(t))
		if err != nil {
			t.Fatal(err)
		}
		p := &syncPeer{id: id, client: client, targetByMaster: map[string]string{}}
		if err := p.attachToCurrentPage(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(client.Close)
		return p
	}
	master := mkPeer("m", masterRec)
	follower := mkPeer("f", followerRec)

	ssvc.mu.Lock()
	ssvc.peers = map[string]*syncPeer{"m": master, "f": follower}
	ssvc.masterID = "m"
	ssvc.state = SyncStateActive
	ssvc.pressed = newPressedState()
	ssvc.mu.Unlock()
	return ssvc, followerRec, master, follower
}

// pressOneKeyAndButton drives one CDP key press and one mouse
// press into the session's pressed state through the real
// dispatch path.
func pressOneKeyAndButton(t *testing.T, ssvc *SyncService, master *syncPeer) {
	t.Helper()
	hit := wininput.Hit{
		IsContent:   true,
		ContentRect: wininput.Rect{X: 0, Y: 0, Width: 1024, Height: 640},
	}
	var lastPress struct {
		button string
		x, y   int32
		at     time.Time
		count  int
	}
	ssvc.dispatchPageEvent(wininput.Event{Kind: wininput.EventKeyDown, VK: 65}, master, hit, ssvc.pressed, &lastPress)
	ssvc.dispatchPageEvent(wininput.Event{Kind: wininput.EventMouseDown, Button: "left", ButtonsMask: 1, X: 10, Y: 10}, master, hit, ssvc.pressed, &lastPress)
	ssvc.pressed.mu.Lock()
	keys, buttons := len(ssvc.pressed.cdpKeys), len(ssvc.pressed.cdpButtons)
	ssvc.pressed.mu.Unlock()
	if keys != 1 || buttons != 1 {
		t.Fatalf("pressed state = %d keys, %d buttons; want 1 and 1", keys, buttons)
	}
}

func assertReleased(t *testing.T, ssvc *SyncService, rec *recordingCDPServer) {
	t.Helper()
	if !rec.sawInput("Input.dispatchKeyEvent", "keyUp") {
		t.Fatal("follower never received the key release")
	}
	if !rec.sawInput("Input.dispatchMouseEvent", "mouseReleased") {
		t.Fatal("follower never received the mouse button release")
	}
	ssvc.pressed.mu.Lock()
	left := len(ssvc.pressed.cdpKeys) + len(ssvc.pressed.cdpButtons) +
		len(ssvc.pressed.nativeKeys) + len(ssvc.pressed.nativeButtons)
	ssvc.pressed.mu.Unlock()
	if left != 0 {
		t.Fatalf("pressed state still holds %d entries after release", left)
	}
}

func TestPauseReleasesPressedInputs(t *testing.T) {
	ssvc, rec, master, _ := releaseTestSession(t)
	pressOneKeyAndButton(t, ssvc, master)
	rec.clear()
	if err := ssvc.PauseSync(""); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, ssvc, rec)

	// After resuming, input flows again and registers afresh.
	if err := ssvc.ResumeSync(""); err != nil {
		t.Fatal(err)
	}
	rec.clear()
	pressOneKeyAndButton(t, ssvc, master)
	if !rec.sawInput("Input.dispatchKeyEvent", "keyDown") {
		t.Fatal("input did not resume after pause/release")
	}
}

func TestStopReleasesPressedInputs(t *testing.T) {
	ssvc, rec, master, _ := releaseTestSession(t)
	pressOneKeyAndButton(t, ssvc, master)
	// A natively pressed key is also tracked and must be let go
	// (the native release itself is a no-op in this environment,
	// but the bookkeeping must clear).
	ssvc.pressed.mu.Lock()
	ssvc.pressed.nativeKeys[0x10] = true
	ssvc.pressed.nativeButtons["right"] = true
	ssvc.pressed.mu.Unlock()
	rec.clear()
	if err := ssvc.StopSync(""); err != nil {
		t.Fatal(err)
	}
	if st := ssvc.GetSyncState(); st.State != SyncStateIdle {
		t.Fatalf("state = %q, want idle", st.State)
	}
	// pressed was released and dropped with the session.
	if !rec.sawInput("Input.dispatchKeyEvent", "keyUp") ||
		!rec.sawInput("Input.dispatchMouseEvent", "mouseReleased") {
		t.Fatal("stop did not release the CDP pressed inputs")
	}
}
