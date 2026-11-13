package service

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Secure subscription fetching (Stage 6 §9). The fetcher is the
// ONLY way subscription bytes enter the app: the frontend never
// downloads subscription URLs.
//
// Defences, in order of the request lifecycle:
//   - scheme pinned to http/https, at the initial URL and at every
//     redirect hop;
//   - the hostname is resolved once, EVERY returned IP is
//     validated against the network policy, and the dial uses the
//     validated IPs directly — the name is never re-resolved at
//     dial time (DNS rebinding cannot swap in a forbidden target);
//   - loopback, unspecified and multicast targets are always
//     refused; private and link-local targets are refused unless
//     the subscription explicitly allows LAN access (and then
//     link-local stays refused);
//   - no environment proxy is consulted (a proxy could bypass the
//     IP policy entirely);
//   - at most 5 redirects, a 20 s overall timeout, and a 10 MiB
//     response cap;
//   - TLS verification is always on.
//
// Error messages carry the scheme+host hint and a failure class
// only — never the full URL, userinfo or query.
const (
	maxSubscriptionBytes     = 10 << 20
	maxSubscriptionRedirects = 5
	subscriptionFetchTimeout = 20 * time.Second
)

// subscriptionFetcher downloads subscription payloads under the
// policy above. The resolver, dialer and TLS roots are fields so
// tests can substitute a loopback test server without weakening
// the production policy path.
type subscriptionFetcher struct {
	resolve   func(ctx context.Context, host string) ([]net.IP, error)
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tlsConfig *tls.Config // nil in production; tests inject test roots
}

func newSubscriptionFetcher() *subscriptionFetcher {
	resolver := &net.Resolver{}
	return &subscriptionFetcher{
		resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			return resolver.LookupIP(ctx, "ip", host)
		},
		dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
	}
}

// urlHintOf returns scheme://host for an already-known URL, for
// use in error messages and the subscriptions.url_hint column.
// It never includes userinfo, path or query.
func urlHintOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// validateSubscriptionURL enforces the scheme/host shape shared by
// subscription creation and fetching.
func validateSubscriptionURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("订阅地址格式无效")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("订阅地址仅支持 http 或 https")
	}
	if u.Hostname() == "" {
		return fmt.Errorf("订阅地址缺少主机名")
	}
	return nil
}

// specialUsePrefixes are address ranges that are NOT ordinary
// public unicast space and are therefore refused in both policy
// modes (Stage 6 review: net.IP.IsPrivate alone does not cover
// them). Each entry names the registry purpose that keeps it out
// of the public target set. The list follows the IANA IPv4/IPv6
// special-purpose registries; allocations the registries mark
// globally reachable (the AS112 and AMT anycast ranges, for
// example) are deliberately NOT listed, and no broad aggregate
// such as 2001::/23 is banned, because it contains globally
// reachable assignments.
var specialUsePrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 791)
	netip.MustParsePrefix("100.64.0.0/10"),   // shared address space / CGNAT (RFC 6598)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF special registry (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 documentation (RFC 5737)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 documentation (RFC 5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 documentation (RFC 5737)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast, deprecated (RFC 7526)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, incl. the limited broadcast (RFC 1112)
	// IPv6
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known prefix (RFC 6052): translated, not a direct target
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use translation prefix (RFC 8215)
	netip.MustParsePrefix("100::/64"),       // discard-only (RFC 6666)
	netip.MustParsePrefix("100:0:0:1::/64"), // dummy prefix (RFC 7600)
	netip.MustParsePrefix("2001::/32"),      // Teredo tunnelling (RFC 4380)
	netip.MustParsePrefix("2001:2::/48"),    // benchmarking (RFC 5180)
	netip.MustParsePrefix("2001:db8::/32"),  // documentation (RFC 3849)
	netip.MustParsePrefix("2001:10::/28"),   // ORCHID, deprecated (RFC 4843)
	netip.MustParsePrefix("3fff::/20"),      // documentation (RFC 9637)
	netip.MustParsePrefix("2002::/16"),      // 6to4 (RFC 3056): tunnels an embedded IPv4 target — refused wholesale so the tunnel spelling cannot bypass the IPv4 policy
	netip.MustParsePrefix("5f00::/16"),      // SRv6 segment IDs (RFC 9603): locator space, not host targets
}

// ipAllowedByPolicy classifies one resolved IP under the
// subscription fetch policy:
//
//   - loopback, unspecified, multicast and link-local addresses
//     are ALWAYS refused;
//   - RFC 1918 IPv4 private space and IPv6 ULA are refused unless
//     the subscription explicitly allows LAN access;
//   - special-use, reserved, documentation, benchmarking and
//     other non-public ranges (specialUsePrefixes) are ALWAYS
//     refused — allowLAN never widens beyond private space;
//   - everything else is ordinary public space and is allowed.
//
// The address is normalised with netip first: an IPv4-mapped IPv6
// address is unmapped to its IPv4 form before any check, so the
// IPv6 spelling of a forbidden IPv4 target cannot slip through.
// globallyReachableExceptions are single addresses INSIDE a
// denied special-use range that the IANA registry nevertheless
// marks Globally Reachable=True; they are carved out of the
// enclosing denial and treated as ordinary public addresses, in
// line with the principle stated on specialUsePrefixes. Both sit
// inside 192.0.0.0/24 (kept denied for every other address):
//
//   - 192.0.0.9  — Port Control Protocol anycast (RFC 7723)
//   - 192.0.0.10 — Traversal Using Relays around NAT anycast (RFC 8155)
var globallyReachableExceptions = []netip.Prefix{
	netip.MustParsePrefix("192.0.0.9/32"),
	netip.MustParsePrefix("192.0.0.10/32"),
}

func ipAllowedByPolicy(ip net.IP, allowLAN bool) bool {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		if v4 := ip.To4(); v4 != nil {
			addr, ok = netip.AddrFromSlice(v4)
		}
		if !ok {
			return false
		}
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsUnspecified() || addr.IsMulticast() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() {
		return false
	}
	if addr.IsPrivate() {
		return allowLAN
	}
	for _, exception := range globallyReachableExceptions {
		if exception.Contains(addr) {
			return true
		}
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

// validateHostIPs resolves host and validates every resulting IP.
// It returns the validated IPs for pinned dialing.
func (f *subscriptionFetcher) validateHostIPs(ctx context.Context, host string, allowLAN bool) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		if !ipAllowedByPolicy(ip, allowLAN) {
			return nil, fmt.Errorf("订阅目标地址不被允许（%s）", host)
		}
		return []net.IP{ip}, nil
	}
	ips, err := f.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("订阅域名解析失败（%s）", host)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("订阅域名没有解析结果（%s）", host)
	}
	for _, ip := range ips {
		if !ipAllowedByPolicy(ip, allowLAN) {
			return nil, fmt.Errorf("订阅域名解析到了不被允许的地址（%s）", host)
		}
	}
	return ips, nil
}

// fetch downloads rawURL under the subscription policy.
func (f *subscriptionFetcher) fetch(ctx context.Context, rawURL string, allowLAN bool) ([]byte, error) {
	if err := validateSubscriptionURL(rawURL); err != nil {
		return nil, err
	}
	// Per-request state: the IPs validated for the host currently
	// being dialed. The transport asks for one host at a time and
	// the CheckRedirect hook re-validates the policy per hop, but
	// the authoritative check is inside the dial hook itself —
	// every new connection re-resolves, re-validates ALL IPs and
	// then dials a validated IP.
	guardedDial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("订阅目标地址无效")
		}
		ips, err := f.validateHostIPs(ctx, host, allowLAN)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ip := range ips {
			conn, err := f.dial(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, fmt.Errorf("连接订阅服务器失败（%s）：%v", urlHintOf(rawURL), dialErrClass(lastErr))
	}
	transport := &http.Transport{
		Proxy:               nil, // never the environment proxy
		DialContext:         guardedDial,
		TLSClientConfig:     f.tlsConfig,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   subscriptionFetchTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxSubscriptionRedirects {
				return fmt.Errorf("订阅重定向次数过多")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("订阅重定向到了不被允许的协议")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("订阅地址格式无效")
	}
	req.Header.Set("User-Agent", "DeBrowser")
	resp, err := client.Do(req)
	if err != nil {
		return nil, sanitizeFetchError(rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("订阅服务器返回了 HTTP %d（%s）", resp.StatusCode, urlHintOf(rawURL))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSubscriptionBytes+1))
	if err != nil {
		return nil, fmt.Errorf("读取订阅内容失败（%s）", urlHintOf(rawURL))
	}
	if len(body) > maxSubscriptionBytes {
		return nil, fmt.Errorf("订阅内容超过 10 MiB 上限")
	}
	return body, nil
}

// dialErrClass reduces a dial failure to a safe class label.
func dialErrClass(err error) string {
	if err == nil {
		return "未知错误"
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "连接超时"
	}
	return "连接被拒绝或不可达"
}

// sanitizeFetchError maps transport errors onto messages that
// cannot leak the full URL (url.Error embeds it). Policy refusals
// produced by our own hooks are passed through — they contain at
// most the host the policy already names.
func sanitizeFetchError(rawURL string, err error) error {
	if ue, ok := err.(*url.Error); ok {
		err = ue.Err
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "不被允许"):
		return err
	case strings.Contains(msg, "重定向"):
		return err
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "tls:"), strings.Contains(msg, "x509"):
		return fmt.Errorf("订阅服务器的 TLS 证书校验失败（%s）", urlHintOf(rawURL))
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return fmt.Errorf("订阅请求超时（%s）", urlHintOf(rawURL))
	case strings.Contains(msg, "解析"):
		return err
	default:
		return fmt.Errorf("订阅获取失败（%s）：%s", urlHintOf(rawURL), dialErrClass(err))
	}
}
