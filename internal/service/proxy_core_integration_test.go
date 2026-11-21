package service

import (
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
	tr := &http.Transport{
		Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)}),
	}
	client := &http.Client{Transport: tr, Timeout: 6 * time.Second}
	resp, err := client.Get("http://127.0.0.1:9/")
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
	deadline := time.Now().Add(8 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = probeVia(port)
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
