package mihomo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// testClient points a Client at an httptest server.
func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return NewClient(port, "test-secret")
}

func TestClientConnectionsParsing(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("missing bearer <redacted>")
		}
		fmt.Fprint(w, `{"downloadTotal": 10, "uploadTotal": 5, "connections": [
			{"id": "c1", "metadata": {"host": "example.com", "destinationIP": "93.184.216.34",
			 "destinationPort": "443", "inboundPort": "21001", "inboundName": "inst-aaaaaaaaaaaa"},
			 "upload": 100, "download": 200, "start": "2026-11-22T10:00:00Z",
			 "chains": ["NodeA", "inst-aaaaaaaaaaaa"], "rule": "RuleSet", "rulePayload": "谷歌服务"},
			{"id": "c2", "metadata": {"host": "", "sniffHost": "sniffed.example", "destinationIP": "10.0.0.1",
			 "destinationPort": "80", "inboundPort": "21002", "inboundName": "inst-bbbbbbbbbbbb"},
			 "upload": 0, "download": 0, "start": "2026-11-22T10:01:00Z",
			 "chains": ["DIRECT"], "rule": "IP-CIDR", "rulePayload": "10.0.0.0/8"}
		]}`)
	}))
	conns, err := c.Connections(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(conns) != 2 {
		t.Fatalf("connections = %d, want 2", len(conns))
	}
	if conns[0].InboundPort != 21001 || conns[0].Host != "example.com" || conns[0].RulePayload != "谷歌服务" {
		t.Fatalf("connection 0 = %+v", conns[0])
	}
	if conns[1].Host != "sniffed.example" {
		t.Fatalf("sniffed host fallback failed: %+v", conns[1])
	}
}

func TestClientStreamLogsParsing(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("level"); got != "info" {
			t.Errorf("level param = %q", got)
		}
		fmt.Fprint(w, "{\"type\":\"info\",\"payload\":\"first\"}\nnot json\n{\"type\":\"warning\",\"payload\":\"second\"}\n")
	}))
	var got []string
	err := c.StreamLogs(context.Background(), "info", func(level, payload string) {
		got = append(got, level+":"+payload)
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "info:first,warning:second" {
		t.Fatalf("streamed lines = %v", got)
	}
}

func TestClientRefreshRuleProvider(t *testing.T) {
	var path string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Method != http.MethodPut {
			t.Errorf("method = %s", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	if err := c.RefreshRuleProvider(context.Background(), "局域网络"); err != nil {
		t.Fatal(err)
	}
	if path != "/providers/rules/局域网络" && !strings.Contains(path, "/providers/rules/") {
		t.Fatalf("path = %q", path)
	}
}
