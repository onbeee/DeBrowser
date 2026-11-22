package mihomo

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	var p int
	if _, err := fmt.Sscanf(port, "%d", &p); err != nil {
		t.Fatal(err)
	}
	return NewClient(p, "test-secret")
}

func TestClientDelay(t *testing.T) {
	var gotPath, gotAuth string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"delay": 123}`))
	}))
	ms, err := c.Delay(context.Background(), "n-abc", "http://www.gstatic.com/generate_204", 5000)
	if err != nil {
		t.Fatal(err)
	}
	if ms != 123 {
		t.Fatalf("delay = %d", ms)
	}
	if !strings.HasPrefix(gotPath, "/proxies/n-abc/delay?") ||
		!strings.Contains(gotPath, "timeout=5000") ||
		!strings.Contains(gotPath, "url=http%3A%2F%2Fwww.gstatic.com%2Fgenerate_204") {
		t.Fatalf("path = %s", gotPath)
	}
	if want := "Bearer " + c.secret; gotAuth != want {
		t.Fatalf("auth header does not carry the client secret")
	}
}

func TestClientDelayFailureStatus(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"An error occurred in the delay test"}`))
	}))
	if _, err := c.Delay(context.Background(), "n-abc", "http://example.com", 5000); err == nil {
		t.Fatal("want error on 503")
	}
}
