package cdp

import (
	"context"
	"encoding/json"
)

// Input dispatch helpers: thin wrappers over the CDP Input
// domain, the standard interface for page-level input. Mouse
// coordinates are viewport CSS pixels of the attached target.

// MouseEvent dispatches one mouse event. typ is one of
// mousePressed, mouseReleased, mouseMoved, mouseWheel. Buttons
// follows the CDP bitmask (1 left, 2 right, 4 middle); button is
// the changed button name ("none", "left", "right", "middle").
func (c *Client) MouseEvent(ctx context.Context, sessionID, typ string, x, y float64, button string, buttons, clickCount int, deltaX, deltaY float64) error {
	params := map[string]any{
		"type": typ, "x": x, "y": y, "button": button,
		"buttons": buttons, "clickCount": clickCount,
	}
	if typ == "mouseWheel" {
		params["deltaX"] = deltaX
		params["deltaY"] = deltaY
	}
	_, err := c.Call(ctx, sessionID, "Input.dispatchMouseEvent", params)
	return err
}

// KeyEvent dispatches one key event. typ is keyDown, keyUp,
// rawKeyDown or char. windowsVirtualKeyCode is the Windows VK
// code; text is set for char events and printable keyDowns.
func (c *Client) KeyEvent(ctx context.Context, sessionID, typ string, vk int, modifiers int, text, key string) error {
	params := map[string]any{
		"type": typ, "windowsVirtualKeyCode": vk,
		"nativeVirtualKeyCode": vk, "modifiers": modifiers,
	}
	if text != "" {
		params["text"] = text
	}
	if key != "" {
		params["key"] = key
	}
	_, err := c.Call(ctx, sessionID, "Input.dispatchKeyEvent", params)
	return err
}

// InsertText inserts a string into the focused element of the
// target, as an IME commit would.
func (c *Client) InsertText(ctx context.Context, sessionID, text string) error {
	_, err := c.Call(ctx, sessionID, "Input.insertText", map[string]any{"text": text})
	return err
}

// Navigate loads a URL in the target's main frame.
func (c *Client) Navigate(ctx context.Context, sessionID, url string) error {
	_, err := c.Call(ctx, sessionID, "Page.navigate", map[string]any{"url": url})
	return err
}

// Reload reloads the target's page.
func (c *Client) Reload(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, sessionID, "Page.reload", nil)
	return err
}

// LayoutViewport returns the CSS-pixel size of the layout
// viewport of the attached target, for coordinate mapping.
func (c *Client) LayoutViewport(ctx context.Context, sessionID string) (width, height float64, err error) {
	raw, err := c.Call(ctx, sessionID, "Page.getLayoutMetrics", nil)
	if err != nil {
		return 0, 0, err
	}
	var out struct {
		LayoutViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"layoutViewport"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, err
	}
	return out.LayoutViewport.ClientWidth, out.LayoutViewport.ClientHeight, nil
}
