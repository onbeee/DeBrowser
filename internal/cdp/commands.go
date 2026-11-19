package cdp

import (
	"context"
	"encoding/json"
	"fmt"
)

// This file holds the typed wrappers for exactly the CDP commands
// Stage 10 uses. It deliberately does not model the whole protocol.

// TargetInfo is the subset of Target.TargetInfo the sync feature
// reads. The URL is navigation information: it lives only in the
// sync session's memory and is never logged or persisted.
type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Attached bool   `json:"attached"`
	OpenerID string `json:"openerId,omitempty"`
}

type attachResult struct {
	SessionID string `json:"sessionId"`
}

// AttachToTarget attaches in flatten mode and returns the session
// ID that identifies the target on this connection from then on.
func (c *Client) AttachToTarget(ctx context.Context, targetID string) (string, error) {
	raw, err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return "", err
	}
	var res attachResult
	if err := json.Unmarshal(raw, &res); err != nil || res.SessionID == "" {
		return "", fmt.Errorf("附加到页面失败：浏览器未返回会话")
	}
	return res.SessionID, nil
}

// GetTargets lists the browser's current targets.
func (c *Client) GetTargets(ctx context.Context) ([]TargetInfo, error) {
	raw, err := c.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var res struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("读取页面列表失败：%w", err)
	}
	return res.TargetInfos, nil
}

// CreateTarget opens a new page target. Background creation is
// requested so the follower's window is not brought to the Windows
// foreground.
func (c *Client) CreateTarget(ctx context.Context, url string, background bool) (string, error) {
	raw, err := c.Call(ctx, "", "Target.createTarget", map[string]any{
		"url": url, "background": background,
	})
	if err != nil {
		return "", err
	}
	var res struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || res.TargetID == "" {
		return "", fmt.Errorf("创建页面失败：浏览器未返回页面标识")
	}
	return res.TargetID, nil
}

// CloseTarget closes one target by ID.
func (c *Client) CloseTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
	return err
}

// ActivateTarget makes one target the active tab in its window.
// Whether this also raises the Windows window is a browser
// behaviour the feature does not rely on (see the sync docs).
func (c *Client) ActivateTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID})
	return err
}

// SetDiscoverTargets toggles target discovery events.
func (c *Client) SetDiscoverTargets(ctx context.Context, discover bool) error {
	_, err := c.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": discover})
	return err
}

// SessionCall is Call with a session ID, named for readability at
// page-command call sites.
func (c *Client) SessionCall(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	return c.Call(ctx, sessionID, method, params)
}
