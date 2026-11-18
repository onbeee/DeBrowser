package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/onbeee/DeBrowser/internal/cdp"
)

// recordingCDP is a fake per-instance CDP endpoint that records
// every command, with optional per-command delay and failure.
type recordingCDP struct {
	ln     net.Listener
	port   int
	mu     sync.Mutex
	calls  []recordedCall
	delay  time.Duration
	failOn string // method substring that returns an error
}

type recordedCall struct {
	Method string
	Params string
}

func newRecordingCDP(t *testing.T) *recordingCDP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &recordingCDP{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/ws","Product":"Fake/1.0"}`, f.port)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		for {
			_, data, err := c.Read(r.Context())
			if err != nil {
				return
			}
			var msg struct {
				ID     int64           `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(data, &msg) != nil || msg.ID == 0 {
				continue
			}
			f.mu.Lock()
			f.calls = append(f.calls, recordedCall{msg.Method, string(msg.Params)})
			delay, failOn := f.delay, f.failOn
			f.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if failOn != "" && strings.Contains(msg.Method, failOn) {
				_ = c.Write(r.Context(), websocket.MessageText,
					[]byte(fmt.Sprintf(`{"id":%d,"error":{"code":-1,"message":"injected failure"}}`, msg.ID)))
				continue
			}
			result := "{}"
			switch msg.Method {
			case "Target.attachToTarget":
				result = `{"sessionId":"s1"}`
			case "Page.getLayoutMetrics":
				result = `{"cssLayoutViewport":{"clientWidth":800,"clientHeight":600},"layoutViewport":{"clientWidth":800,"clientHeight":600}}`
			case "Page.getNavigationHistory":
				result = `{"currentIndex":1,"entries":[{"id":11,"url":"https://a.example/"},{"id":12,"url":"https://b.example/"},{"id":13,"url":"https://c.example/"}]}`
			case "Target.createTarget":
				result = `{"targetId":"t-new"}`
			}
			_ = c.Write(r.Context(), websocket.MessageText,
				[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, msg.ID, result)))
		}
	})
	go func() { _ = http.Serve(ln, mux) }()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *recordingCDP) count(substr string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.Contains(c.Method+" "+c.Params, substr) {
			n++
		}
	}
	return n
}

func (f *recordingCDP) waitFor(t *testing.T, substr string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.count(substr) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d calls matching %q; got %d", want, substr, f.count(substr))
}

func dialWorker(t *testing.T, id string, f *recordingCDP) *followerWorker {
	t.Helper()
	client, err := cdp.Dial(context.Background(), f.port)
	if err != nil {
		t.Fatal(err)
	}
	w := newFollowerWorker(id, client, nil)
	w.queue.push(syncEvent{kind: evInit, targets: []string{"t1"}, index: 0, masterW: 1000, masterH: 800})
	go w.Run()
	t.Cleanup(func() {
		w.Stop()
		<-w.done
		client.Close()
	})
	// Wait for init attach.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.count("attachToTarget") >= 1 {
			return w
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker init did not attach")
	return nil
}

func TestFollowerQueueCoalescesMoves(t *testing.T) {
	q := newFollowerQueue(8)
	q.push(syncEvent{kind: evMouseMove, x: 1})
	q.push(syncEvent{kind: evMouseMove, x: 2})
	q.push(syncEvent{kind: evMouseMove, x: 3})
	if q.len() != 1 {
		t.Fatalf("queue len = %d, want 1 (coalesced)", q.len())
	}
	ev, ok := q.pop()
	if !ok || ev.x != 3 {
		t.Fatalf("pop = %+v, want latest move x=3", ev)
	}
}

func TestFollowerQueueNeverDropsRelease(t *testing.T) {
	q := newFollowerQueue(4)
	for i := 0; i < 4; i++ {
		q.push(syncEvent{kind: evKey, vk: 65, down: true})
	}
	q.push(syncEvent{kind: evKey, vk: 65, down: false}) // release must fit
	found := false
	for q.len() > 0 {
		ev, _ := q.pop()
		if ev.kind == evKey && !ev.down {
			found = true
		}
	}
	if !found {
		t.Fatal("release event was dropped from a full queue")
	}
}

func TestWorkerSlowFollowerDoesNotBlockFast(t *testing.T) {
	slow := newRecordingCDP(t)
	slow.delay = 400 * time.Millisecond
	fast := newRecordingCDP(t)
	ws := dialWorker(t, "slow", slow)
	wf := dialWorker(t, "fast", fast)

	ws.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	wf.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})

	// The fast follower completes while the slow one is still
	// inside its first delayed call.
	fast.waitFor(t, "dispatchKeyEvent", 1)
	if slow.count("dispatchKeyEvent") > 1 {
		t.Fatal("slow follower somehow raced ahead")
	}
	slow.waitFor(t, "dispatchKeyEvent", 1)
}

func TestWorkerStopReleasesPressedState(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	w.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	w.Enqueue(syncEvent{kind: evMouseButton, x: 10, y: 10, button: "left", down: true, buttons: 1, clickCount: 1})
	f.waitFor(t, "keyDown", 1)
	f.waitFor(t, "mousePressed", 1)
	w.Stop()
	<-w.done
	if f.count("keyUp") != 1 {
		t.Fatalf("keyUp calls = %d, want 1", f.count("keyUp"))
	}
	if f.count("mouseReleased") != 1 {
		t.Fatalf("mouseReleased calls = %d, want 1", f.count("mouseReleased"))
	}
}

func TestWorkerReleaseReachesFailedFollower(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	w.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	f.waitFor(t, "keyDown", 1)
	// Fail the follower with a bad navigation.
	f.mu.Lock()
	f.failOn = "Page.navigate"
	f.mu.Unlock()
	w.Enqueue(syncEvent{kind: evNavigate, url: "https://x.example/"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if failed, _ := w.isFailed(); failed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed, _ := w.isFailed(); !failed {
		t.Fatal("worker did not mark itself failed")
	}
	// Even failed, a Release must still deliver the keyUp for the
	// key pressed before the failure.
	w.Release()
	f.waitFor(t, "keyUp", 1)
}

func TestWorkerHistoryDeltaUsesOwnHistory(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	w.Enqueue(syncEvent{kind: evHistoryDelta, delta: -1})
	f.waitFor(t, "navigateToHistoryEntry", 1)
	if f.count(`"entryId":11`) != 1 {
		t.Fatalf("expected navigation to own entry 11, calls: %+v", f.calls)
	}
	// A delta beyond the stack fails the follower with a reason
	// instead of navigating somewhere wrong.
	w2 := dialWorker(t, "f2", f)
	w2.Enqueue(syncEvent{kind: evHistoryDelta, delta: -5})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if failed, reason := w2.isFailed(); failed && strings.Contains(reason, "历史记录") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, reason := w2.isFailed()
	t.Fatalf("out-of-range history delta did not fail with reason, got %q", reason)
}

func TestWorkerMapsCoordinatesByViewport(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	// Master viewport 1000x800 (from init), follower 800x600:
	// a move to the master's center lands at the follower's
	// center (400,300).
	w.Enqueue(syncEvent{kind: evMouseMove, x: 500, y: 400})
	f.waitFor(t, "mouseMoved", 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c.Method, "dispatchMouseEvent") {
			if !strings.Contains(c.Params, `"x":400`) || !strings.Contains(c.Params, `"y":300`) {
				t.Fatalf("mapped move params = %s", c.Params)
			}
			return
		}
	}
	t.Fatal("no mouse event recorded")
}

func TestSyncKeyForVK(t *testing.T) {
	cases := []struct {
		vk       uint32
		key      string
		code     string
		modifier bool
	}{
		{0x41, "a", "KeyA", false},
		{0x35, "5", "Digit5", false},
		{0x0D, "Enter", "Enter", false},
		{0x26, "ArrowUp", "ArrowUp", false},
		{0x70, "F1", "F1", false},
		{0xA2, "Control", "ControlLeft", true},
		{0xBC, ",", "Comma", false},
	}
	for _, tc := range cases {
		key, code, _, mod := syncKeyForVK(tc.vk)
		if key != tc.key || code != tc.code || mod != tc.modifier {
			t.Errorf("vk %#x: got (%q,%q,%v), want (%q,%q,%v)", tc.vk, key, code, mod, tc.key, tc.code, tc.modifier)
		}
	}
	if key, _, _, _ := syncKeyForVK(0xFF); key != "" {
		t.Errorf("unknown vk should map to empty, got %q", key)
	}
}

func TestStartSyncValidatesParticipants(t *testing.T) {
	_, isvc, _ := bookmarkTestServices(t)
	svc := NewSyncService(isvc)
	inst := makeInstance(t, isvc, "not-running")
	if err := svc.StartSync("", inst.ID, []string{"x"}); err == nil {
		t.Fatal("start with a stopped master must fail")
	}
	if err := svc.StartSync("", inst.ID, nil); err == nil {
		t.Fatal("start without followers must fail")
	}
	st, err := svc.GetSyncState("")
	if err != nil || st.Phase != "idle" {
		t.Fatalf("state = %+v, %v", st, err)
	}
	opts, err := svc.ListSyncInstances("")
	if err != nil || len(opts) != 1 || opts[0].Running {
		t.Fatalf("options = %+v, %v", opts, err)
	}
}
