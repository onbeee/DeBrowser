package cdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBrowser is a scripted CDP endpoint: it answers commands from
// a table and can push one event after the first command.
type fakeBrowser struct {
	answers map[string]json.RawMessage
	event   *wireMessage
}

func (f *fakeBrowser) handler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()
	ctx := context.Background()
	sentEvent := false
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil || msg.ID == 0 {
			continue
		}
		result := f.answers[msg.Method]
		if result == nil {
			result = json.RawMessage(`{}`)
		}
		out, _ := json.Marshal(wireMessage{ID: msg.ID, Result: result})
		if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
			return
		}
		if f.event != nil && !sentEvent {
			sentEvent = true
			ev, _ := json.Marshal(f.event)
			_ = conn.Write(ctx, websocket.MessageText, ev)
		}
	}
}

func dialFake(t *testing.T, f *fakeBrowser, handler Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, err := Dial(context.Background(), wsURL, handler)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCallCorrelatesResponsesAndDeliversEvents(t *testing.T) {
	events := make(chan Event, 4)
	fb := &fakeBrowser{
		answers: map[string]json.RawMessage{
			"Target.getTargets": json.RawMessage(`{"targetInfos":[{"targetId":"T1","type":"page","url":"https://example.com/"}]}`),
		},
		event: &wireMessage{Method: "Runtime.bindingCalled", SessionID: "S1",
			Params: json.RawMessage(`{"name":"b","payload":"{}","executionContextId":3}`)},
	}
	c := dialFake(t, fb, func(e Event) { events <- e })

	targets, err := c.GetTargets(context.Background())
	if err != nil {
		t.Fatalf("GetTargets: %v", err)
	}
	if len(targets) != 1 || targets[0].TargetID != "T1" {
		t.Fatalf("targets = %+v", targets)
	}
	select {
	case e := <-events:
		if e.Method != "Runtime.bindingCalled" || e.SessionID != "S1" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("binding event not delivered")
	}
}

func TestAttachReturnsSessionID(t *testing.T) {
	fb := &fakeBrowser{answers: map[string]json.RawMessage{
		"Target.attachToTarget": json.RawMessage(`{"sessionId":"S9"}`),
	}}
	c := dialFake(t, fb, nil)
	sid, err := c.AttachToTarget(context.Background(), "T7")
	if err != nil || sid != "S9" {
		t.Fatalf("attach = %q, %v", sid, err)
	}
}

func TestValidateLoopbackWSURL(t *testing.T) {
	good := []string{"ws://127.0.0.1:9222/devtools/browser/x", "ws://localhost:9222/a", "ws://[::1]:9222/a"}
	for _, u := range good {
		if err := ValidateLoopbackWSURL(u, 9222); err != nil {
			t.Errorf("%s: unexpected error %v", u, err)
		}
	}
	bad := []string{
		"ws://192.168.1.5:9222/a",   // LAN address
		"ws://example.com:9222/a",   // remote host
		"http://127.0.0.1:9222/a",   // wrong scheme
		"ws://127.0.0.1:1234/a",     // wrong port
		"ws://127.0.0.1.evil.com/a", // lookalike host
	}
	for _, u := range bad {
		if err := ValidateLoopbackWSURL(u, 9222); err == nil {
			t.Errorf("%s: expected rejection", u)
		}
	}
}

func TestCloseFailsPendingAndFutureCalls(t *testing.T) {
	fb := &fakeBrowser{answers: map[string]json.RawMessage{}}
	c := dialFake(t, fb, nil)
	_ = c.Close()
	if _, err := c.Call(context.Background(), "", "Target.getTargets", nil); err == nil {
		t.Fatal("call after close must fail")
	}
	select {
	case <-c.Done():
	default:
		t.Fatal("Done must be closed after Close")
	}
}
