package service

import (
	"net"
	"strings"
	"testing"
)

// StartSync argument and state validation: every rejection is a
// readable error (never a panic, never a half-built session),
// and a rejected start leaves the service able to start cleanly.
func TestStartSyncValidationErrors(t *testing.T) {
	fx := newSyncStartFixture(t)

	if err := fx.svc.StartSync("", fx.master.ID, nil); err == nil ||
		!strings.Contains(err.Error(), "请至少选择一个跟随实例") {
		t.Fatalf("no followers error = %v", err)
	}
	if err := fx.svc.StartSync("", fx.master.ID, []string{fx.master.ID}); err == nil ||
		!strings.Contains(err.Error(), "不能重复") {
		t.Fatalf("master-as-follower error = %v", err)
	}
	idle := makeInstance(t, fx.isvc, "sync-idle")
	if err := fx.svc.StartSync("", idle.ID, []string{fx.follower.ID}); err == nil {
		t.Fatal("a non-running master must be rejected")
	}
	if err := fx.svc.StartSync("", fx.master.ID, []string{idle.ID}); err == nil {
		t.Fatal("a non-running follower must be rejected")
	}
	// None of the rejections left a session or a stuck starting
	// flag behind.
	fx.start(t)
	if err := fx.svc.StopSync(""); err != nil {
		t.Fatal(err)
	}
}

// A master that was not launched in sync mode (no CDP port), and
// a master whose CDP endpoint refuses the connection, both fail
// the start with a readable error and leave no session behind.
func TestStartSyncCDPDialFailure(t *testing.T) {
	fx := newSyncStartFixture(t)
	fx.setPort(fx.master.ID, 0) // launched without a sync port
	err := fx.svc.StartSync("", fx.master.ID, []string{fx.follower.ID})
	if err == nil || !strings.Contains(err.Error(), "不是以同步模式启动的") {
		t.Fatalf("no-port error = %v", err)
	}
	// A port that was bound and released: connections are refused.
	ln, lerr := net.Listen("tcp", "127.0.0.1:0")
	if lerr != nil {
		t.Fatal(lerr)
	}
	deadPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	fx.setPort(fx.master.ID, deadPort)
	err = fx.svc.StartSync("", fx.master.ID, []string{fx.follower.ID})
	if err == nil || !strings.Contains(err.Error(), "无法连接主实例") {
		t.Fatalf("dial failure error = %v", err)
	}
	st, err := fx.svc.GetSyncState("")
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "idle" {
		t.Fatalf("phase after failed start = %q, want idle", st.Phase)
	}
	// Restore the endpoint: a fresh start succeeds.
	fx.setPort(fx.master.ID, fx.mCDP.port)
	fx.start(t)
	if err := fx.svc.StopSync(""); err != nil {
		t.Fatal(err)
	}
}

// Pause/Resume/Stop: explicit errors without a session, and the
// phase transitions of a live session are reported truthfully.
func TestPauseResumeStopLifecycle(t *testing.T) {
	fx := newSyncStartFixture(t)
	for name, call := range map[string]func(string) error{
		"pause": fx.svc.PauseSync, "resume": fx.svc.ResumeSync, "stop": fx.svc.StopSync,
	} {
		if err := call(""); err == nil || !strings.Contains(err.Error(), "没有进行中的同步会话") {
			t.Fatalf("%s without session error = %v", name, err)
		}
	}

	fx.start(t)
	st, _ := fx.svc.GetSyncState("")
	if st.Phase != "running" {
		t.Fatalf("phase = %q, want running", st.Phase)
	}
	if err := fx.svc.PauseSync(""); err != nil {
		t.Fatal(err)
	}
	st, _ = fx.svc.GetSyncState("")
	if st.Phase != "paused" {
		t.Fatalf("phase after pause = %q, want paused", st.Phase)
	}
	if err := fx.svc.ResumeSync(""); err != nil {
		t.Fatal(err)
	}
	st, _ = fx.svc.GetSyncState("")
	if st.Phase != "running" {
		t.Fatalf("phase after resume = %q, want running", st.Phase)
	}
	if err := fx.svc.StopSync(""); err != nil {
		t.Fatal(err)
	}
	st, _ = fx.svc.GetSyncState("")
	if st.Phase != "idle" {
		t.Fatalf("phase after stop = %q, want idle", st.Phase)
	}
	starts, stops := fx.captureCounts()
	if starts != 1 || stops != 1 {
		t.Fatalf("capture starts/stops = %d/%d, want 1/1 (no residue)", starts, stops)
	}
}
