package service

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/onbeee/DeBrowser/internal/mihomo"
)

// Real-core integration tests for Stage 6C. They run the actual
// pinned Mihomo binary (DEBROWSER_TEST_MIHOMO must point at the
// genuine official binary; the tests are skipped without it)
// against in-process fake SOCKS5 nodes, each rewriting traffic to
// its own tagged echo server — so a response body proves which
// node carried the traffic, exactly like the Stage 6C PoC bench.
//
// Covered end to end: manual start/stop only, per-instance node
// isolation, NO silent fallback when a bound node disappears,
// binding restore when it returns, reimport validation keeping
// the old file and nodes, binding validation against the live
// core, and refusal to adopt or stop an external core.

func testMihomoBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("DEBROWSER_TEST_MIHOMO")
	if bin == "" {
		t.Skip("DEBROWSER_TEST_MIHOMO not set; real-core tests skipped")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("DEBROWSER_TEST_MIHOMO: %v", err)
	}
	return bin
}

func (f *proxyFixture) installCore(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(testMihomoBinary(t))
	if err != nil {
		t.Fatal(err)
	}
	dst := mihomo.BinaryPath(f.cfg.Layout.Mihomo)
	if err := os.MkdirAll(f.cfg.Layout.Mihomo, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o750); err != nil {
		t.Fatal(err)
	}
}

// startEcho starts a TCP server answering every connection with
// a minimal HTTP 200 response whose body is tag.
func startEcho(t *testing.T, tag string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, _ = io.ReadAll(io.LimitReader(c, 4096)) // the request (or SOCKS payload prelude)
				resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(tag), tag)
				_, _ = c.Write([]byte(resp))
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// startSocksNode starts a minimal SOCKS5 server that ignores the
// requested destination and connects to the given echo port
// instead (the PoC bench trick: the echo tag proves the node).
func startSocksNode(t *testing.T, echoPort int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go handleSocksConn(c, echoPort)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func handleSocksConn(c net.Conn, echoPort int) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil || head[0] != 5 {
		return
	}
	methods := make([]byte, int(head[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return
	}
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 {
		return
	}
	switch req[3] {
	case 1:
		if _, err := io.ReadFull(c, make([]byte, 6)); err != nil {
			return
		}
	case 3:
		lb := make([]byte, 1)
		if _, err := io.ReadFull(c, lb); err != nil {
			return
		}
		if _, err := io.ReadFull(c, make([]byte, int(lb[0])+2)); err != nil {
			return
		}
	case 4:
		if _, err := io.ReadFull(c, make([]byte, 18)); err != nil {
			return
		}
	default:
		return
	}
	up, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort), 3*time.Second)
	if err != nil {
		_, _ = c.Write([]byte{5, 5, 0, 1, 0, 0, 0, 0, 0, 0})
		return
	}
	defer up.Close()
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	_ = c.SetDeadline(time.Time{})
	go func() { _, _ = io.Copy(up, c) }()
	_, _ = io.Copy(c, up)
}

// probeVia fetches through an instance listener and returns the
// echo tag that answered. A rejected/blocked route is an error.
func probeVia(port int) (string, error) {
	return probeViaURL(port, "http://127.0.0.1:9/")
}

func probeViaURL(port int, rawURL string) (string, error) {
	tr := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}),
	}
	client := &http.Client{Transport: tr, Timeout: 6 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status %d body %q", resp.StatusCode, b)
	}
	return string(b), nil
}

// probeWant polls probeVia until it yields want (a tag, or "" for
// "must be blocked") or the deadline passes.
func probeWant(t *testing.T, port int, want string) {
	t.Helper()
	probeWantURL(t, port, "http://127.0.0.1:9/", want)
}

// probeWantURL is probeWant against an explicit target URL (the
// route-mode test needs a target a DIRECT route can actually
// reach, which the port-9 probe target is not).
func probeWantURL(t *testing.T, port int, rawURL, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = probeViaURL(port, rawURL)
		if want == "" && lastErr != nil {
			return
		}
		if want != "" && lastErr == nil && last == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("probe via %d: got %q (err %v), want %q", port, last, lastErr, want)
}

func writeSourceYAML(t *testing.T, path string, nodes ...string) []byte {
	t.Helper()
	var b strings.Builder
	b.WriteString("proxies:\n")
	for _, n := range nodes {
		parts := strings.SplitN(n, "=", 2)
		fmt.Fprintf(&b, "  - {name: %s, type: socks5, server: 127.0.0.1, port: %s}\n", parts[0], parts[1])
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return []byte(b.String())
}

func TestProxyCoreEndToEnd(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	// Bindings default to route mode rule, so the generated
	// configuration carries the shared rule-providers and a
	// GEOIP sub-rule line: seed empty provider caches and the
	// GeoIP database so this test's routing assertions depend
	// only on group selection (every set empty → MATCH).
	seedRuleCaches(t, f.cfg.Layout.Mihomo, false)
	seedGeoIP(t, f.cfg.Layout.Mihomo)

	echoA := startEcho(t, "VIA-A")
	echoB := startEcho(t, "VIA-B")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)

	dir := t.TempDir()
	fileV1 := filepath.Join(dir, "v1.yaml")
	fileV2 := filepath.Join(dir, "v2.yaml")
	fileBad := filepath.Join(dir, "bad.yaml")
	v1Bytes := writeSourceYAML(t, fileV1,
		fmt.Sprintf("NodeA=%d", nodeA), fmt.Sprintf("NodeB=%d", nodeB))
	writeSourceYAML(t, fileV2, fmt.Sprintf("NodeB=%d", nodeB))
	if err := os.WriteFile(fileBad, []byte("proxies: [unclosed\n  broken: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pick := fileV1
	f.svc.SourceFilePicker = func() (string, error) { return pick, nil }

	view, err := f.svc.AddFileSource("op", "本地节点")
	if err != nil {
		t.Fatalf("add file source: %v", err)
	}
	inst1 := f.makeInstance(t, "实例一")
	inst2 := f.makeInstance(t, "实例二")
	b1, err := f.svc.SetBinding("op", inst1.ID, view.ID, "NodeA")
	if err != nil {
		t.Fatalf("bind inst1: %v", err)
	}
	if _, err := f.svc.SetBinding("op", inst2.ID, view.ID, "NodeB"); err != nil {
		t.Fatalf("bind inst2: %v", err)
	}

	// Nothing has started the core: status is stopped and the
	// launch hook refuses bound instances.
	if st := f.svc.GetCoreStatus(); st.State != "stopped" {
		t.Fatalf("status before manual start = %q, want stopped", st.State)
	}
	if _, err := f.svc.ProxyPortForInstance(inst1.ID); err == nil {
		t.Fatal("bound instance must not get a port while the core is stopped")
	}

	// Manual start.
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("StartCore: %v", err)
	}
	if st := f.svc.GetCoreStatus(); st.State != "running" {
		t.Fatalf("status after start = %q, want running", st.State)
	}
	port, err := f.svc.ProxyPortForInstance(inst1.ID)
	if err != nil || port != b1.Port {
		t.Fatalf("ProxyPortForInstance = %d, %v; want %d, nil", port, err, b1.Port)
	}
	probeWant(t, b1.Port, "VIA-A")
	b2, _, err := f.svc.GetBinding(inst2.ID)
	if err != nil {
		t.Fatal(err)
	}
	probeWant(t, b2.Port, "VIA-B")

	// Binding to a node that does not exist is rejected while the
	// core runs, and the recorded binding is unchanged.
	if _, err := f.svc.SetBinding("op", inst1.ID, view.ID, "不存在的节点"); err == nil {
		t.Fatal("binding to a missing node must fail while the core runs")
	}
	if got, _, err := f.svc.GetBinding(inst1.ID); err != nil || got.NodeName != "NodeA" {
		t.Fatalf("binding after failed set = %+v, %v; want NodeA kept", got, err)
	}

	// Reimport without NodeA: inst1 must go silent (REJECT), NOT
	// fall back to NodeB; inst2 is unaffected.
	pick = fileV2
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport v2: %v", err)
	}
	probeWant(t, b1.Port, "")
	probeWant(t, b2.Port, "VIA-B")
	if got, _, err := f.svc.GetBinding(inst1.ID); err != nil || got.NodeState != "missing" {
		t.Fatalf("inst1 node state = %+v, %v; want missing", got, err)
	}

	// Reimport with NodeA back: the original binding serves again.
	pick = fileV1
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport v1: %v", err)
	}
	probeWant(t, b1.Port, "VIA-A")

	// Launch-gate self-heal: park inst1's group on REJECT directly
	// in the core — the drift a core-side provider auto-refresh
	// can leave behind although NodeA exists. The gate must
	// re-apply the recorded selection and hand out the port.
	if err := f.svc.client.SelectNode(context.Background(), mihomo.GroupName(inst1.ID), "REJECT"); err != nil {
		t.Fatalf("park group on REJECT: %v", err)
	}
	probeWant(t, b1.Port, "")
	port, err = f.svc.ProxyPortForInstance(inst1.ID)
	if err != nil || port != b1.Port {
		t.Fatalf("gate after drift = %d, %v; want %d, nil (healed)", port, err, b1.Port)
	}
	probeWant(t, b1.Port, "VIA-A")

	// A group whose node is genuinely gone refuses the launch
	// after the failed heal, instead of starting a browser whose
	// traffic would only be rejected.
	pick = fileV2
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport v2 (gate refusal setup): %v", err)
	}
	if _, err := f.svc.ProxyPortForInstance(inst1.ID); err == nil {
		t.Fatal("bound instance with a missing node must not get a port")
	}
	pick = fileV1
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport v1 (gate refusal restore): %v", err)
	}
	probeWant(t, b1.Port, "VIA-A")

	// Reimporting a broken file fails, keeps the stored copy
	// byte-identical and keeps the old nodes serving.
	pick = fileBad
	if _, err := f.svc.ReimportFileSource("op", view.ID); err == nil {
		t.Fatal("reimporting a broken file must fail")
	}
	src, err := f.store.GetProxySource(t.Context(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(f.cfg.Layout.Mihomo, "sources", src.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(v1Bytes) {
		t.Fatal("stored copy changed after a failed reimport")
	}
	probeWant(t, b1.Port, "VIA-A")

	// Deleting an instance drops its listener and group from the
	// running core at once: the record deletion cascade-removes
	// the binding row, and the deletion hook regenerates and
	// reloads the core configuration.
	if err := f.store.DeleteInstance(context.Background(), inst2.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.RefreshAfterInstanceDeleted(inst2.ID)
	if mihomo.ProbePort(b2.Port) {
		t.Fatal("deleted instance's listener still answers in the running core")
	}
	cfgBytes, err := os.ReadFile(f.svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgBytes), mihomo.GroupName(inst2.ID)) {
		t.Fatal("generated config still contains the deleted instance's group")
	}
	probeWant(t, b1.Port, "VIA-A")

	// Manual stop: listeners close and the hook refuses again.
	if err := f.svc.StopCore("op"); err != nil {
		t.Fatalf("StopCore: %v", err)
	}
	if mihomo.ProbePort(b1.Port) {
		t.Fatal("instance listener still answers after StopCore")
	}
	if _, err := f.svc.ProxyPortForInstance(inst1.ID); err == nil {
		t.Fatal("bound instance must not get a port after StopCore")
	}
}

// TestProxyCoreEditableRulesLive verifies the editable rules
// against the real core: with the core RUNNING and the instance
// untouched, adding a custom top rule, disabling it, and
// disabling a built-in ruleset each take effect on the very next
// request through the standard reload flow. It also checks the
// log stream picks up the core's connection lines once the log
// level is raised to info.
func TestProxyCoreEditableRulesLive(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	seedRuleCaches(t, f.cfg.Layout.Mihomo, true) // lan set claims 127.0.0.0/8 -> DIRECT
	seedGeoIP(t, f.cfg.Layout.Mihomo)

	echoA := startEcho(t, "VIA-A")
	targetPort := startEcho(t, "DIRECT-TARGET")
	nodeA := startSocksNode(t, echoA)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/", targetPort)

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "nodes.yaml")
	writeSourceYAML(t, srcFile, fmt.Sprintf("NodeA=%d", nodeA))
	f.svc.SourceFilePicker = func() (string, error) { return srcFile, nil }
	srcView, err := f.svc.AddFileSource("op", "可编辑规则")
	if err != nil {
		t.Fatal(err)
	}
	inst := f.makeInstance(t, "实例一").ID
	b, err := f.svc.SetBinding("op", inst, srcView.ID, "NodeA") // rule mode (default)
	if err != nil {
		t.Fatal(err)
	}
	// Raise the log level BEFORE the start so the log stream has
	// connection lines to pick up.
	settings, err := f.svc.GetProxySettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.LogLevel = "info"
	if err := f.svc.UpdateProxySettings("op", settings); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.svc.StopCore("op") }()
	// Attach the log stream now: /logs only forwards lines
	// emitted after the subscription, so the first read must
	// happen before the traffic it should capture.
	if _, err := f.svc.GetCoreLogs(0); err != nil {
		t.Fatal(err)
	}

	// Baseline: the LAN set sends the loopback target direct.
	probeWantURL(t, b.Port, targetURL, "DIRECT-TARGET")

	// 1. A custom top rule rejecting the target's /8 takes
	// effect live, ahead of the LAN set's DIRECT.
	rule, err := f.svc.AddCustomRule("op", "IP-CIDR", "127.0.0.0/8", "reject")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := probeViaURL(b.Port, targetURL); err == nil {
		t.Fatalf("the rejecting custom rule must block the target, got body %q", body)
	}

	// 2. Disabling the custom rule restores the direct path.
	if err := f.svc.UpdateCustomRule("op", rule.ID, "IP-CIDR", "127.0.0.0/8", "reject", false); err != nil {
		t.Fatal(err)
	}
	probeWantURL(t, b.Port, targetURL, "DIRECT-TARGET")

	// 3. Deleting the custom rule and disabling the LAN ruleset
	// drops the target through to the final MATCH: the bound
	// node serves it.
	if err := f.svc.DeleteCustomRule("op", rule.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetRulePolicy("op", "局域网络", "disabled"); err != nil {
		t.Fatal(err)
	}
	probeWantURL(t, b.Port, targetURL, "VIA-A")

	// The log stream (level info) must have picked up core log
	// lines by now; poll briefly for the buffer to fill.
	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		view, err := f.svc.GetCoreLogs(0)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range view.Lines {
			if strings.Contains(l.Text, "[TCP]") {
				found = true
			}
		}
		if !found {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("the core log stream delivered no connection lines at info level")
	}
}

func TestProxyCoreExternalConflict(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	bin := testMihomoBinary(t)

	// An external core (different secret) already owns the
	// controller port.
	extDir := t.TempDir()
	extSecret := "external-secret-not-ours"
	extCfg := filepath.Join(extDir, "config.yaml")
	cfgText := fmt.Sprintf("log-level: warning\nmode: rule\nexternal-controller: %q\nsecret: %q\nrules:\n  - \"MATCH,DIRECT\"\n",
		"127.0.0.1:19097", extSecret)
	if err := os.WriteFile(extCfg, []byte(cfgText), 0o640); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-d", extDir, "-f", extCfg)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	extClient := mihomo.NewClient(19097, extSecret)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := extClient.Version(t.Context()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("external core did not come up")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Our status reports the conflict honestly...
	if st := f.svc.GetCoreStatus(); st.State != "error" {
		t.Fatalf("status with external core = %q, want error", st.State)
	}
	// ...starting ours fails without touching the external one...
	if err := f.svc.StartCore("op"); err == nil {
		t.Fatal("StartCore must fail when the controller port is taken by an external core")
	}
	// ...and StopCore (nothing of ours runs) leaves it alive.
	if err := f.svc.StopCore("op"); err != nil {
		t.Fatalf("StopCore: %v", err)
	}
	if _, err := extClient.Version(t.Context()); err != nil {
		t.Fatal("the external core was disturbed by DeBrowser")
	}
}

func TestProxySubscriptionRefreshNoFallback(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)

	echoA := startEcho(t, "VIA-A")
	echoB := startEcho(t, "VIA-B")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)

	var content atomic.Value
	content.Store(fmt.Sprintf("proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: %d}\n  - {name: NodeB, type: socks5, server: 127.0.0.1, port: %d}\n", nodeA, nodeB))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(content.Load().(string)))
	}))
	defer server.Close()

	view, err := f.svc.AddURLSource("op", "订阅", server.URL+"/sub.yaml")
	if err != nil {
		t.Fatalf("add url source: %v", err)
	}
	inst := f.makeInstance(t, "实例")
	b, err := f.svc.SetBinding("op", inst.ID, view.ID, "NodeA")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("StartCore: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()
	probeWant(t, b.Port, "VIA-A")

	// The subscription drops NodeA: after a refresh the instance
	// must go silent, never fall back to NodeB.
	content.Store(fmt.Sprintf("proxies:\n  - {name: NodeB, type: socks5, server: 127.0.0.1, port: %d}\n", nodeB))
	if err := f.svc.RefreshSource("op", view.ID); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	probeWant(t, b.Port, "")
	if got, _, err := f.svc.GetBinding(inst.ID); err != nil || got.NodeState != "missing" {
		t.Fatalf("node state after refresh = %+v, %v; want missing", got, err)
	}
}

// ---------------------------------------------------------------------------
// Final fix round: selection errors, health states, rollbacks
// ---------------------------------------------------------------------------

// forceDelayCheck runs one health-check round for a provider's
// nodes through the core's delay endpoint, using the local echo
// as the check URL (the sandbox has no internet; the production
// path differs only in the URL the core checks against).
func forceDelayCheck(t *testing.T, f *proxyFixture, sourceID string, echoPort int) {
	t.Helper()
	src, err := f.store.GetProxySource(context.Background(), sourceID)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(filepath.Join(f.cfg.Layout.Mihomo, "controller-secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{}
	q.Set("url", fmt.Sprintf("http://127.0.0.1:%d/", echoPort))
	// The echo answers only after its read settles (~3s), so the
	// per-node timeout must comfortably exceed that.
	q.Set("timeout", "9000")
	req, err := http.NewRequest(http.MethodGet,
		"http://127.0.0.1:19097/group/"+url.PathEscape(mihomo.DelayGroupName(src.ProviderName))+"/delay?"+q.Encode(), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delay check: status %d", resp.StatusCode)
	}
}

func bindingState(t *testing.T, f *proxyFixture, instanceID string) string {
	t.Helper()
	view, bound, err := f.svc.GetBinding(instanceID)
	if err != nil || !bound {
		t.Fatalf("get binding: bound=%v err=%v", bound, err)
	}
	return view.NodeState
}

// Node health states come from the core's own data: unchecked is
// neutral ("unknown"), a passed health check is "ok", a failed
// one is "down", and a node gone from its source is "missing" —
// while the recorded binding NEVER changes.
func TestProxyNodeHealthStates(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)

	echoA := startEcho(t, "VIA-A")
	nodeA := startSocksNode(t, echoA)
	// A node whose upstream is dead: the SOCKS handshake works,
	// every connect fails — the node exists but is unhealthy.
	deadLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadPort := deadLn.Addr().(*net.TCPAddr).Port
	_ = deadLn.Close()
	nodeC := startSocksNode(t, deadPort)

	dir := t.TempDir()
	fileV1 := filepath.Join(dir, "s1.yaml")
	fileV2 := filepath.Join(dir, "s2.yaml")
	writeSourceYAML(t, fileV1, fmt.Sprintf("NodeA=%d", nodeA), fmt.Sprintf("NodeC=%d", nodeC))
	writeSourceYAML(t, fileV2, fmt.Sprintf("NodeA=%d", nodeA))
	pick := fileV1
	f.svc.SourceFilePicker = func() (string, error) { return pick, nil }

	view, err := f.svc.AddFileSource("op", "健康状态")
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	inst1 := f.makeInstance(t, "健康实例")
	inst2 := f.makeInstance(t, "故障实例")
	if _, err := f.svc.SetBinding("op", inst1.ID, view.ID, "NodeA"); err != nil {
		t.Fatalf("bind inst1: %v", err)
	}
	if _, err := f.svc.SetBinding("op", inst2.ID, view.ID, "NodeC"); err != nil {
		t.Fatalf("bind inst2: %v", err)
	}
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()

	// Not yet health-checked: neutral, never presented as healthy.
	if got := bindingState(t, f, inst1.ID); got != "unknown" {
		t.Fatalf("unchecked node state = %q, want unknown", got)
	}
	forceDelayCheck(t, f, view.ID, echoA)
	if got := bindingState(t, f, inst1.ID); got != "ok" {
		t.Fatalf("healthy node state = %q, want ok", got)
	}
	if got := bindingState(t, f, inst2.ID); got != "down" {
		t.Fatalf("failing node state = %q, want down", got)
	}

	// NodeC disappears from the source: yellow "missing", and
	// the recorded binding still names NodeC.
	pick = fileV2
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport without NodeC: %v", err)
	}
	if got := bindingState(t, f, inst2.ID); got != "missing" {
		t.Fatalf("vanished node state = %q, want missing", got)
	}
	stored, err := f.store.GetBinding(context.Background(), inst2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.NodeName != "NodeC" {
		t.Fatalf("binding was rewritten to %q; the user's binding must be preserved", stored.NodeName)
	}
}

// A selection failure is an error (never a swallowed warning),
// the failed instance's group is parked on REJECT, other
// instances are unaffected, and a StartCore whose selections
// fail stops the core again instead of reporting success.
func TestProxySelectionFailureHandling(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)

	echoA := startEcho(t, "VIA-A")
	echoB := startEcho(t, "VIA-B")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)
	fileV1 := filepath.Join(t.TempDir(), "s.yaml")
	writeSourceYAML(t, fileV1, fmt.Sprintf("NodeA=%d", nodeA), fmt.Sprintf("NodeB=%d", nodeB))
	f.svc.SourceFilePicker = func() (string, error) { return fileV1, nil }
	view, err := f.svc.AddFileSource("op", "选择失败")
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	inst1 := f.makeInstance(t, "实例甲")
	inst2 := f.makeInstance(t, "实例乙")
	b1, err := f.svc.SetBinding("op", inst1.ID, view.ID, "NodeA")
	if err != nil {
		t.Fatalf("bind inst1: %v", err)
	}
	b2, err := f.svc.SetBinding("op", inst2.ID, view.ID, "NodeB")
	if err != nil {
		t.Fatalf("bind inst2: %v", err)
	}

	var rejectParked []string
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		if node == "NodeA" {
			return fmt.Errorf("injected selection failure")
		}
		if node == "REJECT" {
			rejectParked = append(rejectParked, group)
		}
		return f.svc.client.SelectNode(ctx, group, node)
	}

	// StartCore must fail AND leave no core running.
	if err := f.svc.StartCore("op"); err == nil {
		t.Fatal("StartCore with a failing selection must return an error")
	}
	if st := f.svc.GetCoreStatus(); st.State != "stopped" {
		t.Fatalf("core state after failed StartCore = %q, want stopped", st.State)
	}
	if mihomo.ProbePort(proxyControllerPort) {
		t.Fatal("controller port still answers after failed StartCore; the core was left running")
	}

	// With selections working again the core starts; a direct
	// applySelections failure then names only the failed
	// instance, parks its group on REJECT and changes nothing
	// in the database.
	f.svc.selectOverride = nil
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()
	rejectParked = nil
	f.svc.selectOverride = func(ctx context.Context, group, node string) error {
		if node == "NodeA" {
			return fmt.Errorf("injected selection failure")
		}
		if node == "REJECT" {
			rejectParked = append(rejectParked, group)
		}
		return f.svc.client.SelectNode(ctx, group, node)
	}
	err = f.svc.applySelections(context.Background())
	if err == nil {
		t.Fatal("applySelections must return the selection failure")
	}
	if !strings.Contains(err.Error(), "实例甲") || strings.Contains(err.Error(), "实例乙") {
		t.Fatalf("error should name only the failed instance: %v", err)
	}
	parked := false
	for _, g := range rejectParked {
		if g == mihomo.GroupName(inst1.ID) {
			parked = true
		}
	}
	if !parked {
		t.Fatal("the failed instance's group was not parked on REJECT")
	}
	stored, err := f.store.GetBinding(context.Background(), inst1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.NodeName != "NodeA" {
		t.Fatalf("binding changed to %q after a selection failure", stored.NodeName)
	}
	// The other instance still runs on its own node.
	probeWant(t, b2.Port, "VIA-B")
	_ = b1
}

// A reimport whose provider refresh fails restores the old file
// byte for byte and keeps the old nodes serving; the same
// reimport succeeds once the refresh works again.
func TestProxyReimportRefreshFailureRollsBack(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)

	echoA := startEcho(t, "VIA-A")
	echoB := startEcho(t, "VIA-B")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)
	dir := t.TempDir()
	fileV1 := filepath.Join(dir, "r1.yaml")
	fileV2 := filepath.Join(dir, "r2.yaml")
	v1Bytes := writeSourceYAML(t, fileV1, fmt.Sprintf("NodeA=%d", nodeA), fmt.Sprintf("NodeB=%d", nodeB))
	writeSourceYAML(t, fileV2, fmt.Sprintf("NodeB=%d", nodeB))
	pick := fileV1
	f.svc.SourceFilePicker = func() (string, error) { return pick, nil }
	view, err := f.svc.AddFileSource("op", "回滚来源")
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	inst1 := f.makeInstance(t, "回滚实例")
	b1, err := f.svc.SetBinding("op", inst1.ID, view.ID, "NodeA")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()
	probeWant(t, b1.Port, "VIA-A")

	// The first refresh (the new file) fails; the rollback
	// refresh (the restored old file) succeeds.
	refreshCalls := 0
	f.svc.refreshOverride = func(ctx context.Context, provider string) error {
		refreshCalls++
		if refreshCalls == 1 {
			return fmt.Errorf("injected refresh failure")
		}
		return f.svc.client.RefreshProvider(ctx, provider)
	}
	pick = fileV2
	if _, err := f.svc.ReimportFileSource("op", view.ID); err == nil {
		t.Fatal("reimport with a failing refresh must return an error")
	}
	srcRec, err := f.store.GetProxySource(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(f.svc.sourcesDir(), srcRec.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != string(v1Bytes) {
		t.Fatal("the stored copy was not restored to the old file after the refresh failure")
	}
	probeWant(t, b1.Port, "VIA-A") // old nodes keep serving
	if got := bindingState(t, f, inst1.ID); got == "missing" {
		t.Fatal("NodeA must still exist after the rollback")
	}

	// Once refreshes work, the same reimport goes through.
	f.svc.refreshOverride = nil
	if _, err := f.svc.ReimportFileSource("op", view.ID); err != nil {
		t.Fatalf("reimport after recovery: %v", err)
	}
	probeWant(t, b1.Port, "") // NodeA is gone now: rejected, never switched to NodeB
}

// Adding a subscription whose provider refresh fails rolls the
// whole source back: no row, no provider in the live config.
func TestProxyAddURLSourceRefreshFailureRollsBack(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	nodeA := startSocksNode(t, startEcho(t, "VIA-A"))
	payload := fmt.Sprintf("proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: %d}\n", nodeA)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()

	f.svc.refreshOverride = func(ctx context.Context, provider string) error {
		return fmt.Errorf("injected refresh failure")
	}
	if _, err := f.svc.AddURLSource("op", "失败订阅", srv.URL); err == nil {
		t.Fatal("AddURLSource with a failing refresh must return an error")
	}
	sources, err := f.store.ListProxySources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("rolled-back source left %d rows behind", len(sources))
	}
	cfgText, err := os.ReadFile(f.svc.configPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfgText), "src-") {
		t.Fatal("the live configuration still contains the rolled-back provider")
	}

	f.svc.refreshOverride = nil
	if _, err := f.svc.AddURLSource("op", "正常订阅", srv.URL); err != nil {
		t.Fatalf("add after recovery: %v", err)
	}
	nodes, err := f.svc.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Name != "NodeA" {
		t.Fatalf("nodes after recovery = %+v", nodes)
	}
}

// The throwaway validation core is always reaped: after a
// successful validation, after an unloadable file, and after a
// cancelled context, its controller port is free again.
func TestProxyValidationProcessReaped(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	_ = f.svc.GetCoreStatus() // one-time secret/manager init
	bin := mihomo.BinaryPath(f.cfg.Layout.Mihomo)
	valid := writeSourceYAML(t, filepath.Join(t.TempDir(), "v.yaml"), "NodeA=1")

	assertValidatePortFree := func() {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !mihomo.ProbePort(proxyValidatePort) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("the validation core was left running (port still occupied)")
	}

	n, err := f.svc.countFileNodes(context.Background(), bin, valid)
	if err != nil || n != 1 {
		t.Fatalf("valid file: count=%d err=%v", n, err)
	}
	assertValidatePortFree()

	bogus := []byte("proxies:\n  - {name: X, type: no-such-type, server: 127.0.0.1, port: 1}\n")
	_, _ = f.svc.countFileNodes(context.Background(), bin, bogus)
	assertValidatePortFree()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.svc.countFileNodes(ctx, bin, valid); err == nil {
		t.Fatal("a cancelled validation must return an error")
	}
	assertValidatePortFree()
}

// When the database delete of a source fails, the runtime
// configuration is restored: database and core stay consistent.
func TestProxyDeleteSourceDatabaseFailureKeepsRuntime(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	nodeA := startSocksNode(t, startEcho(t, "VIA-A"))
	fileV1 := filepath.Join(t.TempDir(), "d.yaml")
	writeSourceYAML(t, fileV1, fmt.Sprintf("NodeA=%d", nodeA))
	f.svc.SourceFilePicker = func() (string, error) { return fileV1, nil }
	view, err := f.svc.AddFileSource("op", "删除失败来源")
	if err != nil {
		t.Fatalf("add source: %v", err)
	}
	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("start core: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()

	if _, err := f.db.SQL().Exec("CREATE TRIGGER block_source_delete BEFORE DELETE ON proxy_sources BEGIN SELECT RAISE(ABORT, 'blocked by test'); END;"); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = f.db.SQL().Exec("DROP TRIGGER block_source_delete") }()
	if err := f.svc.DeleteSource("op", view.ID); err == nil {
		t.Fatal("DeleteSource with a failing database delete must return an error")
	}
	if _, err := f.store.GetProxySource(context.Background(), view.ID); err != nil {
		t.Fatalf("the source row must survive its failed delete: %v", err)
	}
	nodes, err := f.svc.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range nodes {
		if n.Name == "NodeA" {
			found = true
		}
	}
	if !found {
		t.Fatal("the runtime configuration lost the source after its failed delete")
	}
}

// A file that fails core validation is rejected before anything
// is archived: no row, no stored copy.
func TestProxyAddFileSourceValidationFailureLeavesNothing(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("proxies:\n  - {name: X, type: no-such-type, server: 127.0.0.1, port: 1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.svc.SourceFilePicker = func() (string, error) { return bad, nil }
	if _, err := f.svc.AddFileSource("op", "坏文件"); err == nil {
		t.Fatal("an unloadable file must be rejected")
	}
	sources, err := f.store.ListProxySources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 0 {
		t.Fatalf("rejected import left %d source rows", len(sources))
	}
	entries, _ := os.ReadDir(f.svc.sourcesDir())
	if len(entries) != 0 {
		t.Fatalf("rejected import left files behind: %v", entries)
	}
}

// seedRuleCaches pre-writes the shared rule-providers' cache
// files in the core workdir so tests never depend on the
// network or on whatever the live rule sets contain today.
// Every set is empty; with lanLoopback the 局域网络 set also
// claims 127.0.0.0/8 — the route-mode test uses that to prove a
// rule-mode request to a loopback target goes DIRECT while a
// global-mode request to the same target still uses its node.
// Tests that only need the sub-rules to fall through to MATCH
// pass false, keeping their routing independent of set content.
func seedRuleCaches(t *testing.T, dir string, lanLoopback bool) {
	t.Helper()
	rulesDir := filepath.Join(dir, "rules")
	if err := os.MkdirAll(rulesDir, 0o750); err != nil {
		t.Fatal(err)
	}
	files := []string{"direct", "block-http-dns", "advertising-lite", "hijacking",
		"privacy", "ai", "google", "twitter", "telegram", "crypto", "microsoft",
		"proxy", "china-max", "lan"}
	for _, name := range files {
		content := "payload: []\n"
		if name == "lan" && lanLoopback {
			content = "payload:\n  - \"IP-CIDR,127.0.0.0/8\"\n"
		}
		if err := os.WriteFile(filepath.Join(rulesDir, name+".yaml"), []byte(content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
}

// seedGeoIP makes sure the core workdir holds the GeoIP
// database: a rule-mode sub-rules set contains a GEOIP line and
// the core refuses to load the configuration at all without the
// database. A copy provided via DEBROWSER_TEST_GEOIP wins;
// otherwise the database is downloaded once from the same
// release the core itself would use. When neither works the
// test skips — the routing behaviour cannot be exercised
// without it.
func seedGeoIP(t *testing.T, dir string) {
	t.Helper()
	dst := filepath.Join(dir, "geoip.metadb")
	if _, err := os.Stat(dst); err == nil {
		return
	}
	if src := os.Getenv("DEBROWSER_TEST_GEOIP"); src != "" {
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("DEBROWSER_TEST_GEOIP unreadable: %v", err)
		}
		if err := os.WriteFile(dst, data, 0o640); err != nil {
			t.Fatal(err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Skipf("geoip.metadb unavailable and download failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("geoip.metadb download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil || len(data) < 1<<20 {
		t.Skipf("geoip.metadb download incomplete: %d bytes, err %v", len(data), err)
	}
	if err := os.WriteFile(dst, data, 0o640); err != nil {
		t.Fatal(err)
	}
}

// Per-instance routing modes against the real core: instance 1
// in rule mode (loopback target claimed by the LAN set goes
// DIRECT), instance 2 in global mode (same target goes through
// its own node), instance 3 in direct mode — simultaneously,
// each isolated from the others; then instance 2's mode is
// switched live and its routing follows.
func TestProxyCoreRouteModes(t *testing.T) {
	f := testProxyFixture(t)
	f.installCore(t)
	seedRuleCaches(t, f.cfg.Layout.Mihomo, true)
	seedGeoIP(t, f.cfg.Layout.Mihomo)

	echoA := startEcho(t, "VIA-A")
	echoB := startEcho(t, "VIA-B")
	targetPort := startEcho(t, "DIRECT-TARGET")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)
	targetURL := fmt.Sprintf("http://127.0.0.1:%d/", targetPort)

	dir := t.TempDir()
	srcFile := filepath.Join(dir, "nodes.yaml")
	writeSourceYAML(t, srcFile,
		fmt.Sprintf("NodeA=%d", nodeA), fmt.Sprintf("NodeB=%d", nodeB))
	f.svc.SourceFilePicker = func() (string, error) { return srcFile, nil }
	srcView, err := f.svc.AddFileSource("op", "路由模式")
	if err != nil {
		t.Fatal(err)
	}
	inst1 := f.makeInstance(t, "实例一").ID
	inst2 := f.makeInstance(t, "实例二").ID
	inst3 := f.makeInstance(t, "实例三").ID
	b1, err := f.svc.SetBinding("op", inst1, srcView.ID, "NodeA") // default: rule
	if err != nil {
		t.Fatal(err)
	}
	b2, err := f.svc.SetBinding("op", inst2, srcView.ID, "NodeB")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetRouteMode("op", inst2, "global"); err != nil {
		t.Fatal(err)
	}
	b3, err := f.svc.SetBinding("op", inst3, srcView.ID, "NodeA")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetRouteMode("op", inst3, "direct"); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.StartCore("op"); err != nil {
		t.Fatalf("StartCore with rule providers: %v", err)
	}
	defer func() { _ = f.svc.StopCore("op") }()

	// The same loopback target, three modes, three outcomes —
	// simultaneously.
	probeWantURL(t, b1.Port, targetURL, "DIRECT-TARGET") // rule: LAN set → DIRECT
	probeWantURL(t, b2.Port, targetURL, "VIA-B")         // global: everything via its node
	probeWantURL(t, b3.Port, targetURL, "DIRECT-TARGET") // direct: never the node
	// Node isolation on the classic probe target still holds:
	// rule mode does not leak the LAN target to its node, and
	// global mode reaches only its own node.
	probeWant(t, b1.Port, "")
	probeWant(t, b2.Port, "VIA-B")

	// Switching instance 2 to direct while the core runs takes
	// effect through the reload; switching back restores the
	// node route.
	if _, err := f.svc.SetRouteMode("op", inst2, "direct"); err != nil {
		t.Fatal(err)
	}
	probeWantURL(t, b2.Port, targetURL, "DIRECT-TARGET")
	if _, err := f.svc.SetRouteMode("op", inst2, "global"); err != nil {
		t.Fatal(err)
	}
	probeWantURL(t, b2.Port, targetURL, "VIA-B")
}
