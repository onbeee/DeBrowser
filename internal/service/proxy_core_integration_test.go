package service

// Real-core integration test for the redesigned proxy module:
// the actual Mihomo binary serves two instances through two
// different nodes via per-instance listeners, an unbound
// instance stays direct, and the launch gate tracks the core's
// real state. Runs only when DEBROWSER_TEST_MIHOMO points at a
// Mihomo binary (CI skips it).

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startEchoServer replies with a fixed marker to any HTTP request.
func startEchoServer(t *testing.T, marker string) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 4096)
				_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, _ = c.Read(buf)
				fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(marker), marker)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// startSocksNode is a minimal SOCKS5 server that forwards every
// connection to the given fixed target (the echo server) — it
// stands in for one real proxy node.
func startSocksNode(t *testing.T, targetPort int) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
				n, err := c.Read(buf)
				if err != nil || n < 2 {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
				n, err = c.Read(buf)
				if err != nil || n < 4 {
					return
				}
				up, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", targetPort), 3*time.Second)
				if err != nil {
					_, _ = c.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer up.Close()
				_, _ = c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
				_ = c.SetReadDeadline(time.Time{})
				go func() { _, _ = io.Copy(up, c) }()
				_, _ = io.Copy(c, up)
			}(conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// probeVia fetches http://127.0.0.1:<echoPort>/ through the
// SOCKS5 listener at viaPort with a hand-rolled SOCKS5 client
// and returns the response body.
func probeVia(t *testing.T, viaPort, echoPort int) string {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", viaPort), 3*time.Second)
	if err != nil {
		t.Fatalf("probe via %d: dial: %v", viaPort, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("probe via %d: greeting: %v", viaPort, err)
	}
	buf := make([]byte, 512)
	if _, err := io.ReadFull(conn, buf[:2]); err != nil {
		t.Fatalf("probe via %d: greeting reply: %v", viaPort, err)
	}
	// CONNECT 127.0.0.1:echoPort
	req := []byte{0x05, 0x01, 0x00, 0x01, 127, 0, 0, 1, byte(echoPort >> 8), byte(echoPort)}
	if _, err := conn.Write(req); err != nil {
		t.Fatalf("probe via %d: connect: %v", viaPort, err)
	}
	if _, err := io.ReadFull(conn, buf[:10]); err != nil {
		t.Fatalf("probe via %d: connect reply: %v", viaPort, err)
	}
	if buf[1] != 0x00 {
		t.Fatalf("probe via %d: connect refused with code %d", viaPort, buf[1])
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.0\r\n\r\n")); err != nil {
		t.Fatalf("probe via %d: write request: %v", viaPort, err)
	}
	body, _ := io.ReadAll(conn)
	return string(body)
}

func TestProxyCoreEndToEnd(t *testing.T) {
	bin := os.Getenv("DEBROWSER_TEST_MIHOMO")
	if bin == "" {
		t.Skip("DEBROWSER_TEST_MIHOMO not set")
	}
	echoA := startEchoServer(t, "VIA-A")
	echoB := startEchoServer(t, "VIA-B")
	nodeA := startSocksNode(t, echoA)
	nodeB := startSocksNode(t, echoB)

	f := testProxyFixture(t)
	// Install the real binary where the manager looks for it.
	f.svc.mu.Lock()
	_ = f.svc.initCoreLocked()
	f.svc.mu.Unlock()
	dst := filepath.Join(f.svc.layout.Mihomo, "mihomo")
	if filepath.Base(bin) != "mihomo" {
		dst = filepath.Join(f.svc.layout.Mihomo, filepath.Base(bin))
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// The manager verifies the pinned hash; the test binary IS
	// the pinned Linux build, so a plain copy is correct.
	if err := os.WriteFile(dst, raw, 0o750); err != nil {
		t.Fatal(err)
	}

	yamlBody := fmt.Sprintf("proxies:\n  - {name: NodeA, type: socks5, server: 127.0.0.1, port: %d}\n  - {name: NodeB, type: socks5, server: 127.0.0.1, port: %d}\n", nodeA, nodeB)
	f.pickFile(t, yamlBody)
	srcView, err := f.svc.AddFileSource("op", "测试配置")
	if err != nil {
		t.Fatal(err)
	}
	_ = srcView
	instA := f.makeInstance(t, "甲")
	instB := f.makeInstance(t, "乙")
	instFree := f.makeInstance(t, "丙")
	nA := f.nodeByName(t, "NodeA")
	nB := f.nodeByName(t, "NodeB")
	bvA, err := f.svc.SetBinding("op", instA.ID, nA.ID)
	if err != nil {
		t.Fatal(err)
	}
	bvB, err := f.svc.SetBinding("op", instB.ID, nB.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Core stopped: the gate refuses bound instances, allows
	// the unbound one direct.
	if _, err := f.svc.ProxyPortForInstance(instA.ID); err == nil {
		t.Fatal("bound instance must be refused while the core is stopped")
	}
	if port, err := f.svc.ProxyPortForInstance(instFree.ID); err != nil || port != 0 {
		t.Fatalf("unbound instance = (%d, %v), want direct", port, err)
	}

	status, err := f.svc.StartCore("op")
	if err != nil {
		t.Fatalf("start core: %v", err)
	}
	if !status.Running {
		t.Fatalf("status = %+v, want running", status)
	}
	t.Cleanup(func() { _, _ = f.svc.StopCore("op") })

	// Each bound instance's port routes through ITS node only.
	if got := probeVia(t, bvA.ListenPort, echoA); !strings.Contains(got, "VIA-A") {
		t.Fatalf("instance A via its port = %q, want VIA-A", got)
	}
	if got := probeVia(t, bvB.ListenPort, echoB); !strings.Contains(got, "VIA-B") {
		t.Fatalf("instance B via its port = %q, want VIA-B", got)
	}
	// Cross-check: A's port never reaches B's echo.
	if got := probeVia(t, bvA.ListenPort, echoB); strings.Contains(got, "VIA-B") {
		t.Fatalf("instance A leaked into node B: %q", got)
	}
	// The gate now hands out exactly the bound ports.
	if port, err := f.svc.ProxyPortForInstance(instA.ID); err != nil || port != bvA.ListenPort {
		t.Fatalf("gate for A = (%d, %v), want %d", port, err, bvA.ListenPort)
	}

	// Moving A to NodeB while the core runs takes effect via
	// reload: A's port now exits through B.
	if _, err := f.svc.SetBinding("op", instA.ID, nB.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := probeVia(t, bvA.ListenPort, echoB); strings.Contains(got, "VIA-B") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rebinding A to NodeB did not take effect on the running core")
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Stop: status is honest and the gate refuses again.
	status, err = f.svc.StopCore("op")
	if err != nil {
		t.Fatal(err)
	}
	if status.Running {
		t.Fatalf("status after stop = %+v", status)
	}
	if _, err := f.svc.ProxyPortForInstance(instA.ID); err == nil {
		t.Fatal("bound instance must be refused after the core stops")
	}
}
