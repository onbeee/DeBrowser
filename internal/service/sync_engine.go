package service

import (
	"context"
	"encoding/json"
	"time"

	"github.com/onbeee/DeBrowser/internal/cdp"
)

// wireMaster installs the master connection's session-level
// handlers: target lifecycle, navigation, dialogs and
// connection loss. Page input events arrive through the
// tracker callback (onMasterEvent).
func (s *SyncService) wireMaster(sess *syncSession) {
	client := sess.masterClient

	client.On("Target.targetCreated", func(ev cdp.Event) {
		var p struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
				OpenerID string `json:"openerId"`
			} `json:"targetInfo"`
		}
		if err := json.Unmarshal(ev.Params, &p); err != nil || p.TargetInfo.Type != "page" {
			return
		}
		tid := p.TargetInfo.TargetID
		// Record first: the URL at creation time is often
		// still empty for a new tab. offerTarget below only
		// creates follower tabs once the URL is known.
		sess.noteTargetURL(tid, p.TargetInfo.URL)
		go func() {
			ctx, cancel := context.WithTimeout(sess.ctx, 10*time.Second)
			defer cancel()
			if err := sess.masterTrack.Attach(ctx, tid); err != nil {
				return
			}
			if !sess.live() {
				return
			}
			// A user-created tab becomes a follower tab
			// exactly once: offerTarget is URL-gated and the
			// worker's ensure path is single-flight, shared
			// with the visibility path.
			sess.offerTarget(tid)
		}()
	})

	// A target's URL typically becomes final only AFTER
	// creation (a new tab starts empty, then resolves to its
	// real URL). That update is what releases the pending
	// follower creation — no timers, no guessing.
	client.On("Target.targetInfoChanged", func(ev cdp.Event) {
		var p struct {
			TargetInfo struct {
				TargetID string `json:"targetId"`
				Type     string `json:"type"`
				URL      string `json:"url"`
			} `json:"targetInfo"`
		}
		if err := json.Unmarshal(ev.Params, &p); err != nil || p.TargetInfo.Type != "page" {
			return
		}
		wasKnown := sess.targetURL(p.TargetInfo.TargetID) != ""
		sess.noteTargetURL(p.TargetInfo.TargetID, p.TargetInfo.URL)
		if !wasKnown {
			sess.offerTarget(p.TargetInfo.TargetID)
		}
	})

	client.On("Target.targetDestroyed", func(ev cdp.Event) {
		var p struct {
			TargetID string `json:"targetId"`
		}
		if err := json.Unmarshal(ev.Params, &p); err != nil {
			return
		}
		sess.forgetTargetURL(p.TargetID)
		sess.forEachFollower(func(w *followerWorker) {
			w.enqueue(PageEvent{Kind: "targetclosed", Source: p.TargetID})
		})
		sess.masterTrack.Forget(p.TargetID)
		sess.mu.Lock()
		if sess.activeTarget == p.TargetID {
			sess.activeTarget = ""
		}
		sess.mu.Unlock()
		sess.refreshActive()
	})

	client.On("Page.frameStartedNavigating", func(ev cdp.Event) {
		s.onMasterNavigation(sess, ev)
	})

	// A JavaScript dialog on the master pauses dispatch until
	// it closes, so no mirrored action can land behind it.
	client.On("Page.javascriptDialogOpening", func(ev cdp.Event) {
		sess.setSystemPause("dialog-master", true)
	})
	client.On("Page.javascriptDialogClosed", func(ev cdp.Event) {
		sess.setSystemPause("dialog-master", false)
	})

	go func() {
		<-client.Done()
		sess.mu.Lock()
		down := !sess.tornDown
		sess.mu.Unlock()
		if down {
			s.teardown(sess, "主实例的调试连接已断开")
		}
	}()
}

// onMasterNavigation translates a confirmed main-frame
// navigation of the master's ACTIVE target into follower
// operations. The URL itself never leaves session memory.
func (s *SyncService) onMasterNavigation(sess *syncSession, ev cdp.Event) {
	var p struct {
		FrameID        string `json:"frameId"`
		LoaderID       string `json:"loaderId"`
		URL            string `json:"url"`
		NavigationType string `json:"navigationType"`
	}
	if err := json.Unmarshal(ev.Params, &p); err != nil {
		return
	}
	sess.mu.Lock()
	active := sess.activeTarget
	sess.mu.Unlock()
	if active == "" || ev.SessionID == "" {
		return
	}
	if sid, ok := sess.masterTrack.Session(active); !ok || sid != ev.SessionID {
		return // not the active target's session
	}
	// Only the main frame drives navigation sync.
	if frameID := sess.masterTrack.mainFrameOf(active); frameID == "" || frameID != p.FrameID {
		return
	}
	if !sess.live() {
		return
	}
	// Short-window dedup by loader: one navigation is handled
	// exactly once even if the event is delivered twice.
	sess.navMu.Lock()
	now := time.Now()
	for k, at := range sess.navSeen {
		if now.Sub(at) > 15*time.Second {
			delete(sess.navSeen, k)
		}
	}
	if p.LoaderID != "" {
		if _, dup := sess.navSeen[p.LoaderID]; dup {
			sess.navMu.Unlock()
			return
		}
		sess.navSeen[p.LoaderID] = now
	}
	sess.navMu.Unlock()

	var ev2 PageEvent
	switch p.NavigationType {
	case "Reload", "ReloadBypassingCache":
		ev2 = PageEvent{Kind: "reload", Source: active}
	case "BackForward":
		// Direction is not carried by this event; the
		// transition is ambiguous, so history moves are
		// driven by the keyboard path instead (Backspace/Alt
		// arrows are ordinary key events). A BackForward
		// navigation initiated from the browser UI is
		// detected via the URL change below as a plain
		// navigation to the same URL the master landed on.
		ev2 = PageEvent{Kind: "navigate", Source: active, Text: p.URL}
	default:
		ev2 = PageEvent{Kind: "navigate", Source: active, Text: p.URL}
	}
	sess.forEachFollower(func(w *followerWorker) {
		w.enqueue(ev2)
	})
}

// onMasterEvent is the tracker's page-event callback.
func (s *SyncService) onMasterEvent(sess *syncSession, targetID string, ev PageEvent) {
	if ev.Kind == "visibility" {
		sess.refreshActive()
		return
	}
	sess.mu.Lock()
	active := sess.activeTarget
	sess.mu.Unlock()
	if targetID != active {
		return // only the confirmed active page is a source
	}
	sess.dispatch(ev)
}

// watchdog re-verifies participant identities: the master
// failing ends the session; a follower failing is isolated.
func (s *SyncService) watchdog(sess *syncSession) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-sess.ctx.Done():
			return
		case <-tick.C:
			if !s.instances.VerifySyncIdentity(sess.master) {
				s.teardown(sess, "主实例已退出或身份无法确认")
				return
			}
			sess.forEachFollower(func(w *followerWorker) {
				if info, ok := s.instances.SyncRuntimeInfo(w.id); !ok || !s.instances.VerifySyncIdentity(info) {
					w.markFailed("实例已退出或身份无法确认")
				}
			})
		}
	}
}
