package service

import (
	"context"
	"time"
)

// This file implements the proxy page's inspection views: the
// core's active connections (per-instance filterable) and its
// live log stream. Both go through the Go backend — the frontend
// never sees the controller secret. The connections view is a
// snapshot per call (the UI polls); the log view is a bounded
// in-memory buffer fed by one forwarding goroutine per core run,
// started lazily on the first read and stopped with the core.

// ConnectionView is one active connection, attributed to the
// instance whose listener it entered through.
type ConnectionView struct {
	ID          string   `json:"id"`
	InstanceID  string   `json:"instanceId"` // "" when not attributable to a bound instance
	Target      string   `json:"target"`     // host (or destination IP) + port
	Rule        string   `json:"rule"`       // e.g. RuleSet, GeoIP, Match, IP-CIDR
	RulePayload string   `json:"rulePayload"`
	Chains      []string `json:"chains"` // outbound path, e.g. [node, inst-group] or [DIRECT]
	Upload      int64    `json:"upload"`
	Download    int64    `json:"download"`
	Start       string   `json:"start"`
}

// ListConnections returns the core's active connections. With an
// instanceID it returns only the connections that entered
// through that instance's listener (the core reports the inbound
// port per connection, and the port identifies the binding
// exactly). A stopped core yields an empty list, not an error —
// the view simply has nothing to show.
func (s *ProxyService) ListConnections(instanceID string) ([]ConnectionView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out := []ConnectionView{}
	if err := s.initCore(); err != nil {
		return out, nil
	}
	if !s.coreRunning(ctx) {
		return out, nil
	}
	bindings, err := s.store.ListBindings(ctx)
	if err != nil {
		return nil, err
	}
	byPort := map[int]string{}
	filterPort := -1
	for _, b := range bindings {
		byPort[b.Port] = b.InstanceID
		if b.InstanceID == instanceID {
			filterPort = b.Port
		}
	}
	if instanceID != "" && filterPort < 0 {
		return out, nil // the instance has no binding: no connections can be its
	}
	conns, err := s.client.Connections(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if filterPort >= 0 && c.InboundPort != filterPort {
			continue
		}
		target := c.Host
		if target == "" {
			target = c.DestinationIP
		}
		if c.DestinationPort != "" {
			target += ":" + c.DestinationPort
		}
		out = append(out, ConnectionView{
			ID: c.ID, InstanceID: byPort[c.InboundPort],
			Target: target, Rule: c.Rule, RulePayload: c.RulePayload,
			Chains: c.Chains, Upload: c.Upload, Download: c.Download, Start: c.Start,
		})
	}
	return out, nil
}

// coreLogLine is one buffered core log line.
type coreLogLine struct {
	Seq   int    `json:"seq"`
	Level string `json:"level"`
	Text  string `json:"text"`
}

// CoreLogsView is one poll of the log buffer: the lines after
// the caller's sequence number, the newest sequence number, and
// whether the stream is currently attached.
type CoreLogsView struct {
	Lines     []coreLogLine `json:"lines"`
	LatestSeq int           `json:"latestSeq"`
	Streaming bool          `json:"streaming"`
}

// maxBufferedLogLines bounds the in-memory log buffer.
const maxBufferedLogLines = 500

// GetCoreLogs returns the core log lines buffered since afterSeq
// (0 = everything buffered). The first call while the core runs
// starts the forwarding stream; lines are only produced at the
// core's configured log level — at the default warning level the
// stream carries warnings and errors, and raising the level in
// the core settings (followed by the automatic reload) is what
// makes connection-level info lines appear.
func (s *ProxyService) GetCoreLogs(afterSeq int) (CoreLogsView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := s.initCore(); err == nil && s.coreRunning(ctx) {
		s.ensureLogStream(ctx)
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	view := CoreLogsView{Lines: []coreLogLine{}, LatestSeq: s.logSeq, Streaming: s.logStreaming}
	for _, l := range s.logLines {
		if l.Seq > afterSeq {
			view.Lines = append(view.Lines, l)
		}
	}
	return view, nil
}

// ensureLogStream starts the forwarding goroutine when it is not
// already running. The stream's level is the configured log
// level at start time.
func (s *ProxyService) ensureLogStream(ctx context.Context) {
	s.logMu.Lock()
	if s.logStreaming {
		s.logMu.Unlock()
		return
	}
	s.logStreaming = true
	streamCtx, cancel := context.WithCancel(context.Background())
	s.logCancel = cancel
	s.logMu.Unlock()

	settings, err := s.loadProxySettings(ctx)
	level := "warning"
	if err == nil && settings.LogLevel != "" {
		level = settings.LogLevel
	}
	go func() {
		_ = s.client.StreamLogs(streamCtx, level, func(lvl, payload string) {
			s.logMu.Lock()
			s.logSeq++
			s.logLines = append(s.logLines, coreLogLine{Seq: s.logSeq, Level: lvl, Text: payload})
			if len(s.logLines) > maxBufferedLogLines {
				s.logLines = append([]coreLogLine(nil), s.logLines[len(s.logLines)-maxBufferedLogLines:]...)
			}
			s.logMu.Unlock()
		})
		s.logMu.Lock()
		s.logStreaming = false
		s.logCancel = nil
		s.logMu.Unlock()
	}()
}

// stopLogStream ends the forwarding goroutine, if any. The
// buffered lines stay readable until the buffer is reused by the
// next stream.
func (s *ProxyService) stopLogStream() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logCancel != nil {
		s.logCancel()
		s.logCancel = nil
	}
	s.logStreaming = false
}
