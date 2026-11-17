// Package cdp is the minimal Chrome DevTools Protocol client
// DeBrowser needs for multi-instance sync (Stage 10): one
// browser-level WebSocket connection per browser instance,
// flat-mode target sessions, and command/response correlation.
// It deliberately implements nothing else — no automation
// framework, no page scripting helpers.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is one protocol event (a message with a method but no id).
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Client is one browser-level CDP connection. All methods are
// safe for concurrent use; calls are correlated by id.
type Client struct {
	conn *websocket.Conn

	mu      sync.Mutex
	nextID  int
	pending map[int]chan callResult

	onEvent func(Event)

	// Protocol events are queued by readLoop and delivered to
	// the handler, in order, by a dedicated goroutine. Running
	// the handler inside readLoop would deadlock any handler
	// that issues a command on this same connection (the
	// response could never be read while the handler waits).
	evMu     sync.Mutex
	evCond   *sync.Cond
	evQueue  []Event
	evClosed bool

	closeOnce sync.Once
	done      chan struct{}
}

// VersionInfo is the subset of /json/version DeBrowser uses.
type VersionInfo struct {
	Product              string `json:"product"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// FetchVersion queries the instance's loopback debugging HTTP
// endpoint. It fails for browsers that are not actually serving
// CDP on the port, which is how unsupported browsers are told
// apart from Chromium ones.
func FetchVersion(ctx context.Context, port int) (VersionInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return VersionInfo{}, err
	}
	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return VersionInfo{}, fmt.Errorf("cdp: version query: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return VersionInfo{}, fmt.Errorf("cdp: version query: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return VersionInfo{}, err
	}
	var v VersionInfo
	if err := json.Unmarshal(body, &v); err != nil {
		return VersionInfo{}, fmt.Errorf("cdp: version response: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return VersionInfo{}, errors.New("cdp: version response has no WebSocket URL")
	}
	return v, nil
}

// Dial connects to the browser-level WebSocket of the instance
// on the given loopback port.
func Dial(ctx context.Context, port int) (*Client, error) {
	v, err := FetchVersion(ctx, port)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, v.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp: websocket dial: %w", err)
	}
	c := &Client{
		conn:    conn,
		pending: map[int]chan callResult{},
		done:    make(chan struct{}),
	}
	c.evCond = sync.NewCond(&c.evMu)
	go c.readLoop()
	go c.eventLoop()
	return c, nil
}

// SetEventHandler registers the receiver for protocol events
// (target lifecycle, page navigation). It replaces any previous
// handler and must be set before events matter.
func (c *Client) SetEventHandler(fn func(Event)) {
	c.mu.Lock()
	c.onEvent = fn
	c.mu.Unlock()
}

func (c *Client) readLoop() {
	defer c.failAll(errors.New("cdp: connection closed"))
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			return
		}
		var msg struct {
			ID        *int            `json:"id"`
			Method    string          `json:"method"`
			SessionID string          `json:"sessionId"`
			Params    json.RawMessage `json:"params"`
			Result    json.RawMessage `json:"result"`
			Error     *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != nil {
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.mu.Unlock()
			if ok {
				if msg.Error != nil {
					ch <- callResult{err: fmt.Errorf("cdp: %s", msg.Error.Message)}
				} else {
					ch <- callResult{result: msg.Result}
				}
			}
			continue
		}
		if msg.Method != "" {
			c.enqueueEvent(Event{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params})
		}
	}
}

// enqueueEvent appends an event for the delivery goroutine. It
// only takes a short-lived lock, so readLoop is never blocked by
// handler work.
func (c *Client) enqueueEvent(ev Event) {
	c.evMu.Lock()
	if !c.evClosed {
		c.evQueue = append(c.evQueue, ev)
		c.evCond.Signal()
	}
	c.evMu.Unlock()
}

// eventLoop delivers queued events to the registered handler in
// arrival order. It exits when the connection is gone; a handler
// running at that moment finishes on its own — its calls fail
// fast on the closed connection rather than hanging.
func (c *Client) eventLoop() {
	for {
		c.evMu.Lock()
		for len(c.evQueue) == 0 && !c.evClosed {
			c.evCond.Wait()
		}
		if c.evClosed {
			c.evMu.Unlock()
			return
		}
		ev := c.evQueue[0]
		c.evQueue = c.evQueue[1:]
		c.evMu.Unlock()

		c.mu.Lock()
		handler := c.onEvent
		c.mu.Unlock()
		if handler != nil {
			handler(ev)
		}
	}
}

func (c *Client) closeEvents() {
	c.evMu.Lock()
	c.evClosed = true
	c.evCond.Broadcast()
	c.evMu.Unlock()
}

func (c *Client) failAll(err error) {
	c.closeEvents()
	c.mu.Lock()
	pending := c.pending
	c.pending = map[int]chan callResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- callResult{err: err}
	}
	c.closeOnce.Do(func() { close(c.done) })
}

// Done closes when the connection is gone.
func (c *Client) Done() <-chan struct{} { return c.done }

// Call sends one command, optionally inside a target session,
// and waits for its response.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan callResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"id": id, "method": method}
	if sessionID != "" {
		req["sessionId"] = sessionID
	}
	if params != nil {
		req["params"] = params
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = c.conn.Write(writeCtx, websocket.MessageText, data)
	cancel()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("cdp: write %s: %w", method, err)
	}
	select {
	case res := <-ch:
		return res.result, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("cdp: connection closed")
	}
}

// Close tears the connection down.
func (c *Client) Close() {
	c.closeEvents()
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
	c.closeOnce.Do(func() { close(c.done) })
}

// BrowserVersion returns the browser product string, proving the
// endpoint is a real Chromium CDP service.
func (c *Client) BrowserVersion(ctx context.Context) (string, error) {
	raw, err := c.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Product string `json:"product"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	return v.Product, nil
}

// Target is one entry of Target.getTargets.
type Target struct {
	ID    string `json:"targetId"`
	Type  string `json:"type"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Targets lists all current targets of the browser.
func (c *Client) Targets(ctx context.Context) ([]Target, error) {
	raw, err := c.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []Target `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.TargetInfos, nil
}

// Attach opens a flat-mode session to a target.
func (c *Client) Attach(ctx context.Context, targetID string) (string, error) {
	raw, err := c.Call(ctx, "", "Target.attachToTarget", map[string]any{
		"targetId": targetID, "flatten": true,
	})
	if err != nil {
		return "", err
	}
	var out struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.SessionID == "" {
		return "", errors.New("cdp: attach returned no session")
	}
	return out.SessionID, nil
}

// CreateTarget opens a new tab with the given URL.
func (c *Client) CreateTarget(ctx context.Context, url string) (string, error) {
	raw, err := c.Call(ctx, "", "Target.createTarget", map[string]any{"url": url})
	if err != nil {
		return "", err
	}
	var out struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.TargetID, nil
}

// CloseTarget closes one tab.
func (c *Client) CloseTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
	return err
}

// ActivateTarget brings one tab to the front of its window.
func (c *Client) ActivateTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID})
	return err
}

// EnableTargetEvents subscribes to target lifecycle events on
// the browser session.
func (c *Client) EnableTargetEvents(ctx context.Context) error {
	_, err := c.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	return err
}
