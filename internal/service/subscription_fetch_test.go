package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"

	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fetchFixture wires a fetcher to a test HTTP server: DNS resolves
// every host to a public IP, and the raw dialer connects to the
// test server regardless of the requested (validated) address —
// the standard seam for exercising the policy without a network.
type fetchFixture struct {
	fetcher  *subscriptionFetcher
	dialedTo []string
	resolve  map[string][]net.IP
}

func newFetchFixture(t *testing.T, handler http.Handler) (*fetchFixture, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	fx := &fetchFixture{resolve: map[string][]net.IP{}}
	f := newSubscriptionFetcher()
	f.resolve = func(_ context.Context, host string) ([]net.IP, error) {
		if ips, ok := fx.resolve[host]; ok {
			return ips, nil
		}
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
		fx.dialedTo = append(fx.dialedTo, addr)
		d := &net.Dialer{}
		return d.DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "http://"))
	}
	fx.fetcher = f
	return fx, srv
}

func TestFetchSuccessPublicHost(t *testing.T) {
	fx, _ := newFetchFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("proxies: []"))
	}))
	body, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/feed?token=abc", false)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(body) != "proxies: []" {
		t.Fatalf("body = %q", body)
	}
	// The dial used the validated public IP, never the hostname
	// and never an unvalidated address.
	if len(fx.dialedTo) != 1 || !strings.HasPrefix(fx.dialedTo[0], "93.184.216.34:") {
		t.Fatalf("dialedTo = %v", fx.dialedTo)
	}
}

func TestFetchRejectsLoopbackAndPrivate(t *testing.T) {
	fx, _ := newFetchFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	fx.resolve["subs.example.com"] = []net.IP{net.ParseIP("127.0.0.1")}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", false); err == nil ||
		!strings.Contains(err.Error(), "不被允许") {
		t.Fatalf("loopback resolve: err = %v", err)
	}
	if len(fx.dialedTo) != 0 {
		t.Fatalf("dialed despite refusal: %v", fx.dialedTo)
	}
	// Private target: refused without allowLAN, allowed with it.
	fx.resolve["subs.example.com"] = []net.IP{net.ParseIP("192.168.1.20")}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", false); err == nil {
		t.Fatal("private target accepted without allowLAN")
	}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", true); err != nil {
		t.Fatalf("private target with allowLAN: %v", err)
	}
	// Even with allowLAN, loopback stays refused — including as a
	// literal IP URL.
	if _, err := fx.fetcher.fetch(context.Background(), "http://127.0.0.1:8080/x", true); err == nil {
		t.Fatal("loopback literal accepted with allowLAN")
	}
	// Mixed resolution (one public, one private) is refused whole.
	fx.resolve["subs.example.com"] = []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("10.0.0.5")}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", false); err == nil {
		t.Fatal("mixed resolution accepted")
	}
}

func TestFetchRedirectPolicy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mid", http.StatusFound)
	})
	mux.HandleFunc("/mid", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/final", http.StatusFound)
	})
	mux.HandleFunc("/final", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok-body"))
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/to-local", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:9/secret", http.StatusFound)
	})
	fx, _ := newFetchFixture(t, mux)
	body, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/start", false)
	if err != nil || string(body) != "ok-body" {
		t.Fatalf("redirect chain: body=%q err=%v", body, err)
	}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/loop", false); err == nil ||
		!strings.Contains(err.Error(), "重定向") {
		t.Fatalf("redirect loop: err = %v", err)
	}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/to-local", false); err == nil ||
		!strings.Contains(err.Error(), "不被允许") {
		t.Fatalf("redirect to loopback: err = %v", err)
	}
}

func TestFetchOversizeAndStatusAndTimeout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(make([]byte, maxSubscriptionBytes+1))
	})
	mux.HandleFunc("/oops", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
	})
	fx, _ := newFetchFixture(t, mux)
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/big", false); err == nil ||
		!strings.Contains(err.Error(), "超过") {
		t.Fatalf("oversize: err = %v", err)
	}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/oops", false); err == nil ||
		!strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("status: err = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := fx.fetcher.fetch(ctx, "http://subs.example.com/slow", false); err == nil ||
		!strings.Contains(err.Error(), "超时") {
		t.Fatalf("timeout: err = %v", err)
	}
}

func TestFetchErrorsNeverLeakURLSecrets(t *testing.T) {
	fx, _ := newFetchFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	raw := "https://user:TOPSECRET@127.0.0.1:1/feed?token=TOPSECRET"
	_, err := fx.fetcher.fetch(context.Background(), raw, false)
	if err == nil {
		t.Fatal("loopback URL accepted")
	}
	if strings.Contains(err.Error(), "TOPSECRET") || strings.Contains(err.Error(), "/feed") {
		t.Fatalf("error leaks URL material: %v", err)
	}
	if _, err := fx.fetcher.fetch(context.Background(), "ftp://subs.example.com/x", false); err == nil ||
		!strings.Contains(err.Error(), "http") {
		t.Fatalf("scheme: err = %v", err)
	}
}

func TestFetchTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("tls-body"))
	}))
	t.Cleanup(srv.Close)
	newTLSFetcher := func(withRoots bool) *subscriptionFetcher {
		f := newSubscriptionFetcher()
		f.resolve = func(_ context.Context, _ string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		f.dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{}
			return d.DialContext(ctx, "tcp", strings.TrimPrefix(srv.URL, "https://"))
		}
		if withRoots {
			pool := x509.NewCertPool()
			pool.AddCert(srv.Certificate())
			f.tlsConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		}
		return f
	}
	// Without the test root the self-signed test certificate must
	// fail verification (production behaviour: TLS always on).
	if _, err := newTLSFetcher(false).fetch(context.Background(),
		"https://example.com/feed", false); err == nil ||
		!strings.Contains(err.Error(), "TLS") {
		t.Fatalf("untrusted TLS: err = %v", err)
	}
	body, err := newTLSFetcher(true).fetch(context.Background(), "https://example.com/feed", false)
	if err != nil {
		t.Fatalf("trusted TLS fetch: %v", err)
	}
	if string(body) != "tls-body" {
		t.Fatalf("body = %q", body)
	}
}

func TestIPAllowedByPolicy(t *testing.T) {
	cases := []struct {
		ip       string
		allowLAN bool
		want     bool
	}{
		{"8.8.8.8", false, true},
		{"127.0.0.1", true, false},
		{"::1", true, false},
		{"0.0.0.0", true, false},
		{"224.0.0.1", true, false},
		{"192.168.0.10", false, false},
		{"192.168.0.10", true, true},
		{"10.1.2.3", true, true},
		{"169.254.1.1", true, false},
		{"fe80::1", true, false},
		{"fd00::1", false, false},
		{"fd00::1", true, true},
	}
	for _, c := range cases {
		ip := net.ParseIP(c.ip)
		if got := ipAllowedByPolicy(ip, c.allowLAN); got != c.want {
			t.Errorf("ipAllowedByPolicy(%s, %v) = %v, want %v", c.ip, c.allowLAN, got, c.want)
		}
	}
}
