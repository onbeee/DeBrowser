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
	"github.com/onbeee/DeBrowser/internal/uia"
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
	conns  map[*websocket.Conn]bool
}

// dropAll severs every live WebSocket connection, simulating
// the browser going away mid-session.
func (f *recordingCDP) dropAll() {
	f.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		_ = c.CloseNow()
	}
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
	f := &recordingCDP{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, conns: make(map[*websocket.Conn]bool)}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/ws","Product":"Fake/1.0"}`, f.port)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns[c] = true
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			delete(f.conns, c)
			f.mu.Unlock()
		}()
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
			case "Target.getTargets":
				result = `{"targetInfos":[{"targetId":"t1","type":"page","title":"Example","url":"https://example.com/"}]}`
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

// dialRecording returns a CDP client connected to a fake
// endpoint, for tests that drive a worker manually.
func dialRecording(t *testing.T, f *recordingCDP) (*cdp.Client, error) {
	t.Helper()
	return cdp.Dial(context.Background(), f.port)
}

func dialWorker(t *testing.T, id string, f *recordingCDP) *followerWorker {
	t.Helper()
	client, err := cdp.Dial(context.Background(), f.port)
	if err != nil {
		t.Fatal(err)
	}
	w := newFollowerWorker(id, client, nil)
	w.queue.push(syncEvent{kind: evInit, bindings: map[string]string{"m1": "t1"}, currentTarget: "m1", masterW: 1000, masterH: 800})
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

// A queue full of critical events admits no more critical
// events — and must SAY so (pushOverload) rather than silently
// dropping a press or release and corrupting input state. The
// worker turns the overload into a reported follower failure
// plus a release of injected state.
func TestFollowerQueueCriticalOverloadIsSignalled(t *testing.T) {
	q := newFollowerQueue(3)
	for i := 0; i < 3; i++ {
		if r := q.push(syncEvent{kind: evKey, vk: 65, down: true}); r != pushOK {
			t.Fatalf("push %d = %v, want pushOK", i, r)
		}
	}
	if r := q.push(syncEvent{kind: evKey, vk: 66, down: true}); r != pushOverload {
		t.Fatalf("critical push into full queue = %v, want pushOverload", r)
	}
	if r := q.push(syncEvent{kind: evKey, vk: 65, down: false}); r != pushOverload {
		t.Fatalf("release push into full queue = %v, want pushOverload (signalled, not dropped)", r)
	}
	if q.len() != 3 {
		t.Fatalf("queue len = %d, want 3 (contents untouched)", q.len())
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

// ---- queue ordering (review fix: no cross-boundary coalescing) ------

func TestFollowerQueueMoveCoalesceStopsAtButtonBoundary(t *testing.T) {
	q := newFollowerQueue(8)
	q.push(syncEvent{kind: evMouseMove, x: 1})
	q.push(syncEvent{kind: evMouseButton, down: true, button: "left"})
	q.push(syncEvent{kind: evMouseMove, x: 2})
	q.push(syncEvent{kind: evMouseMove, x: 3}) // coalesces with x=2 only
	if q.len() != 3 {
		t.Fatalf("queue len = %d, want 3 (move before the press must survive)", q.len())
	}
	ev1, _ := q.pop()
	ev2, _ := q.pop()
	ev3, _ := q.pop()
	if ev1.kind != evMouseMove || ev1.x != 1 {
		t.Fatalf("first = %+v, want the pre-press move x=1", ev1)
	}
	if ev2.kind != evMouseButton || !ev2.down {
		t.Fatalf("second = %+v, want the press", ev2)
	}
	if ev3.kind != evMouseMove || ev3.x != 3 {
		t.Fatalf("third = %+v, want latest trailing move x=3", ev3)
	}
}

func TestFollowerQueueMoveCoalesceStopsAtReleaseBoundary(t *testing.T) {
	q := newFollowerQueue(8)
	q.push(syncEvent{kind: evMouseMove, x: 1})
	q.push(syncEvent{kind: evMouseButton, down: false, button: "left"})
	q.push(syncEvent{kind: evMouseMove, x: 2})
	q.push(syncEvent{kind: evMouseMove, x: 3})
	if q.len() != 3 {
		t.Fatalf("queue len = %d, want 3", q.len())
	}
	var kinds []syncEventKind
	var lastX float64
	for q.len() > 0 {
		ev, _ := q.pop()
		kinds = append(kinds, ev.kind)
		if ev.kind == evMouseMove {
			lastX = ev.x
		}
	}
	if kinds[0] != evMouseMove || kinds[1] != evMouseButton || kinds[2] != evMouseMove || lastX != 3 {
		t.Fatalf("order = %v lastX=%v, want [move button move] with x=3", kinds, lastX)
	}
}

func TestFollowerQueuePreservesMixedOrder(t *testing.T) {
	q := newFollowerQueue(16)
	want := []syncEvent{
		{kind: evKey, vk: 65, down: true},
		{kind: evKey, vk: 65, down: false},
		{kind: evKey, vk: 66, down: true},
		{kind: evMouseButton, down: true, button: "left"},
		{kind: evMouseWheel, deltaY: -120},
		{kind: evMouseButton, down: false, button: "left"},
		{kind: evKey, vk: 66, down: false},
	}
	for _, ev := range want {
		q.push(ev)
	}
	for i, w := range want {
		got, ok := q.pop()
		if !ok || got.kind != w.kind || got.vk != w.vk || got.down != w.down {
			t.Fatalf("position %d: got %+v, want %+v", i, got, w)
		}
	}
}

func TestFollowerQueueFullEvictsMovesOnly(t *testing.T) {
	q := newFollowerQueue(4)
	q.push(syncEvent{kind: evMouseMove, x: 1})
	q.push(syncEvent{kind: evMouseButton, down: true, button: "left"})
	q.push(syncEvent{kind: evMouseMove, x: 2})
	q.push(syncEvent{kind: evKey, vk: 65, down: true})
	// Full. A critical event evicts the OLDEST MOVE — never the
	// press or the key.
	if r := q.push(syncEvent{kind: evMouseButton, down: false, button: "left"}); r != pushOK {
		t.Fatalf("critical push = %v, want pushOK via move eviction", r)
	}
	var seq []string
	for q.len() > 0 {
		ev, _ := q.pop()
		switch ev.kind {
		case evMouseMove:
			seq = append(seq, "move")
		case evMouseButton:
			if ev.down {
				seq = append(seq, "down")
			} else {
				seq = append(seq, "up")
			}
		case evKey:
			seq = append(seq, "key")
		}
	}
	got := strings.Join(seq, ",")
	if got != "down,move,key,up" {
		t.Fatalf("sequence = %q, want down,move,key,up (oldest move evicted, pair intact)", got)
	}
}

func TestWorkerOverloadFailsFollowerAndReleases(t *testing.T) {
	slow := newRecordingCDP(t)
	slow.delay = 250 * time.Millisecond
	w := dialWorker(t, "overloaded", slow)
	other := newRecordingCDP(t)
	wOther := dialWorker(t, "healthy", other)

	w.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	slow.waitFor(t, "keyDown", 1) // worker is inside the slow call now
	// Fill the queue with critical events while the worker is
	// busy, then one more: overload must fail THIS follower.
	for i := 0; i < 300; i++ {
		w.Enqueue(syncEvent{kind: evKey, key: "b", code: "KeyB", vk: 66, down: true, text: "b"})
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if failed, reason := w.isFailed(); failed && strings.Contains(reason, "事件过多") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if failed, reason := w.isFailed(); !failed || !strings.Contains(reason, "事件过多") {
		t.Fatalf("overloaded follower state = %v %q, want failed with overload reason", failed, reason)
	}
	// The pressed key is released even though the follower
	// failed (Release jumps the queue).
	slow.waitFor(t, "keyUp", 1)
	// The other follower is unaffected.
	wOther.Enqueue(syncEvent{kind: evKey, key: "a", code: "KeyA", vk: 65, down: true, text: "a"})
	other.waitFor(t, "keyDown", 1)
	if failed, _ := wOther.isFailed(); failed {
		t.Fatal("healthy follower was affected by the overloaded one")
	}
}

// ---- tab operations by target id (review fix: no positions) --------

func TestWorkerTabOpsUseBindingsNotPositions(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	var opFails []string
	w.onOpFail = func(id, op, reason string) { opFails = append(opFails, op+":"+reason) }

	// Create: counterpart tab is created and bound, NOT attached.
	w.Enqueue(syncEvent{kind: evCreateTab, masterTarget: "m2"})
	f.waitFor(t, "createTarget", 1)
	if f.count("attachToTarget") != 1 {
		t.Fatalf("attach calls = %d, want 1 (creation must not attach)", f.count("attachToTarget"))
	}
	// Activate the bound tab: ActivateTarget + attach.
	w.Enqueue(syncEvent{kind: evActivateTab, masterTarget: "m2"})
	f.waitFor(t, "activateTarget", 1)
	f.waitFor(t, "attachToTarget", 2)
	// Activate an UNBOUND tab: reported, never guessed, and the
	// follower is NOT failed.
	w.Enqueue(syncEvent{kind: evActivateTab, masterTarget: "mX"})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(opFails) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(opFails) != 1 || !strings.Contains(opFails[0], "切换标签") {
		t.Fatalf("opFails = %v, want one tab-switch report", opFails)
	}
	if failed, _ := w.isFailed(); failed {
		t.Fatal("unbound activation must not fail the follower")
	}
	if f.count("activateTarget") != 1 {
		t.Fatal("an unbound activation reached the browser")
	}
	// Close the bound tab, then activating it reports unbound.
	w.Enqueue(syncEvent{kind: evCloseTab, masterTarget: "m2"})
	f.waitFor(t, "closeTarget", 1)
	w.Enqueue(syncEvent{kind: evActivateTab, masterTarget: "m2"})
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(opFails) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(opFails) != 2 {
		t.Fatalf("opFails = %v, want a second report after close", opFails)
	}
}

// ---- UIA semantic ops through the worker (review fix 4) -------------

func TestWorkerUIOpExecutesAndReportsPerFollower(t *testing.T) {
	f := newRecordingCDP(t)
	w := dialWorker(t, "f1", f)
	w.hwnd = 4242
	var executed []string
	w.uiExec = func(hwnd uintptr, op uia.UIOp) error {
		if hwnd != 4242 {
			t.Errorf("uiExec hwnd = %d, want 4242", hwnd)
		}
		executed = append(executed, op.Action)
		if op.Action == "collapse" {
			return fmt.Errorf("uia: no unique control matches the request")
		}
		return nil
	}
	var opFails []string
	w.onOpFail = func(id, op, reason string) { opFails = append(opFails, op+":"+reason) }

	cmd := func(action string) syncEvent {
		return syncEvent{kind: evUIOp, uiOp: &uiOpCommand{
			Action: action, Spec: uia.MenuButtonSpec(), Label: "打开浏览器菜单",
		}}
	}
	w.Enqueue(cmd("expand"))
	w.Enqueue(cmd("collapse"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(opFails) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(executed) != 2 {
		t.Fatalf("executed = %v, want both ops attempted", executed)
	}
	if len(opFails) != 1 || !strings.Contains(opFails[0], "打开浏览器菜单") {
		t.Fatalf("opFails = %v, want the failed op reported by label", opFails)
	}
	if failed, _ := w.isFailed(); failed {
		t.Fatal("a failed UIA op must not fail the follower")
	}
	// No window: reported, executor never called.
	w.hwnd = 0
	w.Enqueue(cmd("expand"))
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(opFails) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(opFails) != 2 || len(executed) != 2 {
		t.Fatalf("opFails = %v executed = %v, want window-missing report without execution", opFails, executed)
	}
}
