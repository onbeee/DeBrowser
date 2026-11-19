package cdp

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
)

// fakeCDPServer is a minimal recording CDP endpoint: an HTTP
// /json/version plus a WebSocket that answers the commands the
// client uses and can emit events on demand.
type fakeCDPServer struct {
	ln     net.Listener
	port   int
	mu     sync.Mutex
	calls  []string
	conn   *websocket.Conn
	closed chan struct{}
}

func newFakeCDPServer(t *testing.T) *fakeCDPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeCDPServer{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, closed: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"webSocketDebuggerUrl":"ws://127.0.0.1:%d/ws","Product":"FakeChrome/1.0"}`, f.port)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = c
		f.mu.Unlock()
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
			f.calls = append(f.calls, msg.Method)
			f.mu.Unlock()
			result := "{}"
			switch msg.Method {
			case "Target.getTargets":
				result = `{"targetInfos":[{"targetId":"t1","type":"page","url":"https://a.example/","attached":false}]}`
			case "Target.attachToTarget":
				result = `{"sessionId":"s1"}`
			}
			_ = c.Write(r.Context(), websocket.MessageText,
				[]byte(fmt.Sprintf(`{"id":%d,"result":%s}`, msg.ID, result)))
		}
	})
	go func() { _ = http.Serve(ln, mux) }()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeCDPServer) emit(t *testing.T, method string, params string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.mu.Lock()
		c := f.conn
		f.mu.Unlock()
		if c != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := c.Write(ctx, websocket.MessageText,
				[]byte(fmt.Sprintf(`{"method":%q,"params":%s}`, method, params)))
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("client never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientCallAndTargets(t *testing.T) {
	f := newFakeCDPServer(t)
	ctx := context.Background()
	client, err := Dial(ctx, f.port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	targets, err := client.Targets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].TargetID != "t1" {
		t.Fatalf("targets = %+v", targets)
	}
}

// The event handler runs on the delivery goroutine: it may call
// back into the same client without deadlocking the read loop.
func TestEventHandlerMayCallClient(t *testing.T) {
	f := newFakeCDPServer(t)
	ctx := context.Background()
	client, err := Dial(ctx, f.port)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	done := make(chan error, 1)
	client.OnEvent(func(ev Event) {
		if ev.Method != "Target.targetDestroyed" {
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := client.Targets(cctx)
		done <- err
	})
	f.emit(t, "Target.targetDestroyed", `{"targetId":"t1"}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handler call failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event handler call deadlocked")
	}
}

func TestFetchVersionRejectsNonCDP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_, _, err = FetchVersion(context.Background(), ln.Addr().(*net.TCPAddr).Port)
	if err == nil {
		t.Fatal("expected an error for a non-CDP endpoint")
	}
}

// failAll can be entered concurrently — Close() racing the read
// loop's failure path when a connection dies during teardown.
// The done channel must still close exactly once: the previous
// check-then-close select panicked with "close of closed
// channel" under that race (caught by the service race suite).
func TestFailAllConcurrentClosesDoneOnce(t *testing.T) {
	for i := 0; i < 100; i++ {
		c := &Client{
			done:    make(chan struct{}),
			pending: make(map[int64]*pendingCall),
		}
		c.evCond = sync.NewCond(&c.evMu)
		var wg sync.WaitGroup
		for g := 0; g < 8; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c.failAll(fmt.Errorf("boom"))
			}()
		}
		wg.Wait()
		select {
		case <-c.Done():
		default:
			t.Fatal("done channel not closed after failAll")
		}
	}
}
