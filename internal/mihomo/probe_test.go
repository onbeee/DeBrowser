package mihomo

import (
	"io"
	"net"
	"testing"
	"time"
)

// TestProbeListenerDistinguishesSquatter pins the port-conflict
// semantics: a bare TCP dial (ProbePort) is satisfied by ANY
// program holding the port, while ProbeListener requires the
// SOCKS5 greeting reply only a Mihomo mixed listener gives. A
// silent squatter must read as "not our listener" — that is
// what lets the launch gate and the bindings view report the
// conflict instead of a healthy listener.
func TestProbeListenerDistinguishesSquatter(t *testing.T) {
	// Silent squatter: accepts and holds connections, never speaks.
	squat, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer squat.Close()
	go func() {
		for {
			c, err := squat.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_, _ = io.Copy(io.Discard, c)
				c.Close()
			}(c)
		}
	}()
	squatPort := squat.Addr().(*net.TCPAddr).Port
	if !ProbePort(squatPort) {
		t.Fatal("ProbePort must report the occupied port as answering")
	}
	if ProbeListener(squatPort) {
		t.Fatal("ProbeListener must reject a silent squatter")
	}

	// SOCKS5-speaking listener: answers the greeting with 05 00.
	socks, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer socks.Close()
	go func() {
		for {
			c, err := socks.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 3)
				if _, err := io.ReadFull(c, buf); err != nil {
					return
				}
				_, _ = c.Write([]byte{0x05, 0x00})
			}(c)
		}
	}()
	socksPort := socks.Addr().(*net.TCPAddr).Port
	if !ProbeListener(socksPort) {
		t.Fatal("ProbeListener must accept a SOCKS5-speaking listener")
	}

	// Nothing there at all.
	empty, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	emptyPort := empty.Addr().(*net.TCPAddr).Port
	empty.Close()
	if ProbeListener(emptyPort) {
		t.Fatal("ProbeListener must reject a port with no listener")
	}
}
