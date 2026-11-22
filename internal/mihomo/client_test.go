package mihomo

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// testClient builds a Client pointed at a test server (the port
// is parsed back out of the server URL).
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
