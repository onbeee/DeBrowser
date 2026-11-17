package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/onbeee/DeBrowser/internal/model"
)

// serveFakeCDP runs a minimal CDP endpoint on the exact loopback
// port an instance was launched with, so StartSync can complete
// its real connect path in tests.
func serveFakeCDP(t *testing.T, port int) {
	t.Helper()
	mux := http.NewServeMux()
	var wsURL string
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"product": "Chrome/130.0.0.0", "webSocketDebuggerUrl": wsURL,
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
			if err := conn.Write(r.Context(), websocket.MessageText, out); err != nil {
				return
			}
		}
	})
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("fake CDP listen on %d: %v", port, err)
	}
	wsURL = fmt.Sprintf("ws://127.0.0.1:%d/devtools/browser/fake", port)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

func syncTestServices(t *testing.T) (*SyncService, *InstanceService, *fakeLauncher) {
	t.Helper()
	fl := newFakeLauncher()
	isvc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	return NewSyncService(isvc, nil), isvc, fl
}

func mustStartInstance(t *testing.T, isvc *InstanceService, name string) model.Instance {
	t.Helper()
	inst, err := isvc.CreateInstance("", CreateInstanceInput{Name: name, BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if err := isvc.StartInstance("", inst.ID); err != nil {
		t.Fatal(err)
	}
	return inst
}

func TestStartSyncValidation(t *testing.T) {
	ssvc, isvc, _ := syncTestServices(t)
	a := mustStartInstance(t, isvc, "sync-a")
	b := mustStartInstance(t, isvc, "sync-b")
	defer func() {
		_ = isvc.StopInstance("", a.ID)
		_ = isvc.StopInstance("", b.ID)
	}()

	if err := ssvc.StartSync("", a.ID, nil); err == nil {
		t.Fatal("start without followers must fail")
	}
	if err := ssvc.StartSync("", a.ID, []string{a.ID}); err == nil {
		t.Fatal("master as its own follower must fail")
	}
	if err := ssvc.StartSync("", a.ID, []string{b.ID, b.ID}); err == nil {
		t.Fatal("duplicate follower must fail")
	}
	if err := ssvc.StartSync("", "missing", []string{b.ID}); err == nil {
		t.Fatal("unknown master must fail")
	}
	// Both instances run but nothing serves their CDP ports: the
	// start must fail rather than pretend sync is live.
	err := ssvc.StartSync("", a.ID, []string{b.ID})
	if err == nil || !strings.Contains(err.Error(), "控制连接") {
		t.Fatalf("start without a CDP endpoint = %v, want a connection failure", err)
	}
	if st := ssvc.GetSyncState(); st.State != SyncStateIdle {
		t.Fatalf("state after failed start = %q, want idle", st.State)
	}
}

func TestStartSyncRequiresRunning(t *testing.T) {
	ssvc, isvc, _ := syncTestServices(t)
	a := mustStartInstance(t, isvc, "sync-run")
	defer func() { _ = isvc.StopInstance("", a.ID) }()
	stopped, err := isvc.CreateInstance("", CreateInstanceInput{Name: "sync-stopped", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	err = ssvc.StartSync("", a.ID, []string{stopped.ID})
	if err == nil || !strings.Contains(err.Error(), "未在运行") {
		t.Fatalf("start with stopped follower = %v, want a not-running failure", err)
	}
}

func TestSyncSessionLifecycle(t *testing.T) {
	ssvc, isvc, _ := syncTestServices(t)
	a := mustStartInstance(t, isvc, "life-a")
	b := mustStartInstance(t, isvc, "life-b")
	defer func() {
		_ = isvc.StopInstance("", a.ID)
		_ = isvc.StopInstance("", b.ID)
	}()
	serveFakeCDP(t, isvc.instanceCDPPort(a.ID))
	serveFakeCDP(t, isvc.instanceCDPPort(b.ID))

	// Capture is unavailable off Windows; StartSync surfaces
	// that instead of faking a session on this platform. On
	// Windows the same call proceeds to an active session.
	err := ssvc.StartSync("", a.ID, []string{b.ID})
	if err != nil {
		if !strings.Contains(err.Error(), "输入捕获") {
			t.Fatalf("start error = %v, want the capture failure on this platform", err)
		}
		if st := ssvc.GetSyncState(); st.State != SyncStateIdle {
			t.Fatalf("state = %q after capture failure, want idle", st.State)
		}
		return
	}
	st := ssvc.GetSyncState()
	if st.State != SyncStateActive || st.MasterID != a.ID || len(st.Followers) != 1 {
		t.Fatalf("state = %+v, want active with master and one follower", st)
	}
	if err := ssvc.PauseSync(""); err != nil {
		t.Fatal(err)
	}
	if st := ssvc.GetSyncState(); st.State != SyncStatePaused {
		t.Fatalf("state = %q, want paused", st.State)
	}
	if err := ssvc.ResumeSync(""); err != nil {
		t.Fatal(err)
	}
	if err := ssvc.StopSync(""); err != nil {
		t.Fatal(err)
	}
	if st := ssvc.GetSyncState(); st.State != SyncStateIdle {
		t.Fatalf("state = %q, want idle", st.State)
	}
	// Stopping the browsers is NOT part of stopping sync.
	if got := statusOf(t, isvc, a.ID); got != model.StatusRunning {
		t.Fatalf("master status after StopSync = %v, want running", got)
	}
}

func TestListSyncInstancesAvailability(t *testing.T) {
	ssvc, isvc, _ := syncTestServices(t)
	a := mustStartInstance(t, isvc, "list-a")
	defer func() { _ = isvc.StopInstance("", a.ID) }()
	stopped, err := isvc.CreateInstance("", CreateInstanceInput{Name: "list-stopped", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	infos, err := ssvc.ListSyncInstances()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]SyncInstanceInfo{}
	for _, info := range infos {
		byID[info.ID] = info
	}
	if !byID[a.ID].CDPAvailable {
		t.Fatalf("running instance from this build must be CDP-available: %+v", byID[a.ID])
	}
	if byID[stopped.ID].CDPAvailable || byID[stopped.ID].CDPReason == "" {
		t.Fatalf("stopped instance must be unavailable with a reason: %+v", byID[stopped.ID])
	}
}
