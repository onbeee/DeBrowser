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

// TestIPAllowedByPolicySpecialRanges extends the policy table
// with the special-use ranges the Stage 6 review required to be
// refused explicitly (in BOTH allowLAN modes), including
// IPv4-mapped IPv6 spellings of forbidden IPv4 targets.
func TestIPAllowedByPolicySpecialRanges(t *testing.T) {
	alwaysDeny := []string{
		// CGNAT shared address space (RFC 6598)
		"100.64.0.1", "100.127.255.254",
		// Documentation TEST-NETs (RFC 5737)
		"192.0.2.1", "198.51.100.1", "203.0.113.1",
		// Benchmarking (RFC 2544)
		"198.18.0.1", "198.19.255.254",
		// IETF special registry + "this network" + reserved
		"192.0.0.1", "0.1.2.3", "240.0.0.1", "255.255.255.255",
		// IPv6 special ranges
		"2001:db8::1",      // documentation
		"2001::1",          // Teredo
		"2001:2::1",        // benchmarking
		"100::1",           // discard-only
		"64:ff9b::8.8.8.8", // NAT64 well-known prefix
		"3fff::1",          // documentation
		// Second SSRF fix round: remaining registry gaps.
		"64:ff9b:1::1",      // NAT64 local-use translation (RFC 8215)
		"100:0:0:1::1",      // dummy prefix (RFC 7600)
		"2001:10::1",        // ORCHID, deprecated (RFC 4843)
		"2002:0808:0808::1", // 6to4 embedding 8.8.8.8 — tunnel spelling refused wholesale
		"192.88.99.1",       // 6to4 relay anycast, deprecated (RFC 7526)
		"5f00::1",           // SRv6 segment IDs (RFC 9603)
		// IPv4-mapped IPv6 spellings must be judged as IPv4
		"::ffff:127.0.0.1",    // loopback
		"::ffff:192.168.0.10", // RFC1918 (deny without LAN)
		"::ffff:100.64.0.1",   // CGNAT
		"::ffff:8.8.8.8",      // public — handled below, not in alwaysDeny
	}
	for _, raw := range alwaysDeny[:len(alwaysDeny)-1] {
		ip := net.ParseIP(raw)
		if ip == nil {
			t.Fatalf("bad test IP %q", raw)
		}
		if ipAllowedByPolicy(ip, false) {
			t.Errorf("%s allowed with allowLAN=false", raw)
		}
		if ipAllowedByPolicy(ip, true) && raw != "::ffff:192.168.0.10" {
			t.Errorf("%s allowed with allowLAN=true", raw)
		}
	}
	// The mapped RFC1918 address follows the LAN rule after
	// unmapping: refused without, allowed with.
	mappedPrivate := net.ParseIP("::ffff:192.168.0.10")
	if ipAllowedByPolicy(mappedPrivate, false) {
		t.Error("mapped RFC1918 allowed without allowLAN")
	}
	if !ipAllowedByPolicy(mappedPrivate, true) {
		t.Error("mapped RFC1918 refused with allowLAN (unmapping broken)")
	}
	// A mapped PUBLIC address stays allowed in both modes.
	mappedPublic := net.ParseIP("::ffff:8.8.8.8")
	if !ipAllowedByPolicy(mappedPublic, false) || !ipAllowedByPolicy(mappedPublic, true) {
		t.Error("mapped public address refused")
	}
	// IPv6 private/link-local/loopback follow their rules.
	if ipAllowedByPolicy(net.ParseIP("fd00::5"), false) || !ipAllowedByPolicy(net.ParseIP("fd00::5"), true) {
		t.Error("ULA did not follow the allowLAN rule")
	}
	if ipAllowedByPolicy(net.ParseIP("fe80::5"), true) {
		t.Error("IPv6 link-local allowed with allowLAN")
	}
	if ipAllowedByPolicy(net.ParseIP("::1"), true) {
		t.Error("IPv6 loopback allowed with allowLAN")
	}
	// Ordinary public addresses are NOT caught by the special
	// table.
	for _, raw := range []string{"1.1.1.1", "8.8.4.4", "93.184.216.34", "2606:4700:4700::1111", "2001:4860:4860::8888"} {
		if !ipAllowedByPolicy(net.ParseIP(raw), false) {
			t.Errorf("public address %s refused", raw)
		}
	}
}

// TestFetchRejectsSpecialRangesWithoutDialing: at the fetch
// level, a DNS answer containing any special-use address refuses
// the whole host and NO dial is ever attempted — including the
// mixed public+special case.
func TestFetchRejectsSpecialRangesWithoutDialing(t *testing.T) {
	fx, _ := newFetchFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	specials := []string{"100.64.0.1", "192.0.2.1", "198.18.0.1", "203.0.113.9", "2001:db8::1",
		// Second SSRF fix round additions.
		"64:ff9b:1::1", "100:0:0:1::1", "2001:10::1", "2002:0808:0808::1", "192.88.99.1"}
	for _, raw := range specials {
		fx.dialedTo = nil
		fx.resolve["subs.example.com"] = []net.IP{net.ParseIP(raw)}
		if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", true); err == nil {
			t.Errorf("%s: fetch succeeded", raw)
		}
		if len(fx.dialedTo) != 0 {
			t.Errorf("%s: dial attempted despite refusal: %v", raw, fx.dialedTo)
		}
	}
	// Mixed public + special: whole resolution refused, no dial.
	fx.dialedTo = nil
	fx.resolve["subs.example.com"] = []net.IP{net.ParseIP("93.184.216.34"), net.ParseIP("100.64.0.1")}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", false); err == nil {
		t.Fatal("mixed public+special resolution accepted")
	}
	if len(fx.dialedTo) != 0 {
		t.Fatalf("dial attempted for mixed resolution: %v", fx.dialedTo)
	}
	// An IPv4-mapped loopback in the DNS answer is refused too.
	fx.dialedTo = nil
	fx.resolve["subs.example.com"] = []net.IP{net.ParseIP("::ffff:127.0.0.1")}
	if _, err := fx.fetcher.fetch(context.Background(), "http://subs.example.com/x", true); err == nil {
		t.Fatal("mapped loopback resolution accepted")
	}
	if len(fx.dialedTo) != 0 {
		t.Fatalf("dial attempted for mapped loopback: %v", fx.dialedTo)
	}
}
