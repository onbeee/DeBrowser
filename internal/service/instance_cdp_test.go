package service

import (
	"strconv"
	"strings"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Stage 10: launches carry a per-instance loopback CDP port, and
// the port is queryable while the instance runs.
func TestLaunchAllocatesLoopbackCDPPort(t *testing.T) {
	fl := newFakeLauncher()
	svc, _, db := testRuntimeService(t, fl)
	registerFakeBrowser(t, db, "reg-a", "fake-browser.exe")
	a, err := svc.CreateInstance("", CreateInstanceInput{Name: "cdp-a", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateInstance("", CreateInstanceInput{Name: "cdp-b", BrowserID: "reg-a"})
	if err != nil {
		t.Fatal(err)
	}
	if p := svc.instanceCDPPort(a.ID); p != 0 {
		t.Fatalf("port before start = %d, want 0", p)
	}
	if err := svc.StartInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.StartInstance("", b.ID); err != nil {
		t.Fatal(err)
	}
	pa, pb := svc.instanceCDPPort(a.ID), svc.instanceCDPPort(b.ID)
	if pa <= 0 || pb <= 0 {
		t.Fatalf("ports = %d, %d, want both > 0", pa, pb)
	}
	if pa == pb {
		t.Fatalf("two instances share CDP port %d", pa)
	}
	// The launch arguments carry the debugging switches with the
	// same port the service reports, bound to loopback only.
	found := false
	for _, la := range fl.lastArgs {
		joined := strings.Join(la, " ")
		if strings.Contains(joined, "--remote-debugging-address=127.0.0.1") &&
			(strings.Contains(joined, "--remote-debugging-port="+strconv.Itoa(pa)) || strings.Contains(joined, "--remote-debugging-port="+strconv.Itoa(pb))) {
			found = true
		}
		if strings.Contains(joined, "--remote-debugging-address=0.0.0.0") {
			t.Fatal("debugging address must never be a public interface")
		}
	}
	if !found {
		t.Fatal("no launch carried the loopback debugging arguments")
	}
	// After a stop the port is gone with the process.
	if err := svc.StopInstance("", a.ID); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, svc, a.ID, model.StatusStopped)
	if p := svc.instanceCDPPort(a.ID); p != 0 {
		t.Fatalf("port after stop = %d, want 0", p)
	}
	if err := svc.StopInstance("", b.ID); err != nil {
		t.Fatal(err)
	}
}
