package service

import "net"

// Stage 10 runtime lookups on the instance service: the sync
// service (same package) reads the per-launch facts it needs — the
// CDP debugging port and the OS process ID — from the instance
// runtime's in-memory tracking. Nothing here is persisted: a
// process the app did not launch in this run has no port and
// cannot join a sync session until the instance is restarted.

// allocateLoopbackPort reserves an ephemeral loopback port and
// releases it for the browser to bind. Zero means allocation
// failed; the launch then proceeds without a debugging port.
func allocateLoopbackPort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0
	}
	defer func() { _ = ln.Close() }()
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		return addr.Port
	}
	return 0
}

// instanceCDPPort returns the debugging port of the instance's
// tracked, still-running process.
func (s *InstanceService) instanceCDPPort(id string) (int, bool) {
	s.rtMu.Lock()
	defer s.rtMu.Unlock()
	w, ok := s.procs[id]
	if !ok || w.exited || w.cdpPort <= 0 {
		return 0, false
	}
	return w.cdpPort, true
}

// instanceProcessID returns the OS process ID of the instance's
// tracked, still-running process (used to find its window).
func (s *InstanceService) instanceProcessID(id string) (int, bool) {
	s.rtMu.Lock()
	defer s.rtMu.Unlock()
	w, ok := s.procs[id]
	if !ok || w.exited || w.proc == nil {
		return 0, false
	}
	return w.proc.PID(), true
}
