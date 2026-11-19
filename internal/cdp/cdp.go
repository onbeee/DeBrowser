// Package cdp is the minimal Chrome DevTools Protocol client
// DeBrowser needs for multi-instance sync (Stage 10 v2): one
// browser-level WebSocket connection per browser instance,
// command calls with id correlation, and protocol events.
//
// Event delivery is deliberately decoupled from the read loop:
// events are queued and delivered by a dedicated goroutine, so
// an event handler may itself issue commands on the same client
// (for example re-attaching after a target is destroyed) without
// deadlocking the loop that must read those commands' responses.
//
// The client never touches page content: it transports input
// events, target management and navigation commands only.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Event is one protocol event notification.
type Event struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// Handler consumes protocol events in arrival order.
type Handler func(Event)

type pendingCall struct {
	ch chan callResult
}

type callResult struct {
	result json.RawMessage
	err    error
}

// Client is one browser-level CDP connection.
type Client struct {
	conn *websocket.Conn
	done chan struct{}

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]*pendingCall
	handler Handler

	evMu     sync.Mutex
	evCond   *sync.Cond
	evQueue  []Event
	evClosed bool

	closeOnce sync.Once
	doneOnce  sync.Once
}

// FetchVersion queries the HTTP discovery endpoint of a Chromium
// remote debugging port and returns its webSocketDebuggerUrl.
// Only loopback ports are ever passed by callers.
func FetchVersion(ctx context.Context, port int) (wsURL string, product string, err error) {
	reqCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("cdp: version endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("cdp: version endpoint: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", "", err
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
		Product              string `json:"Product"`
		Browser              string `json:"Browser"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", "", fmt.Errorf("cdp: version endpoint: invalid response: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", "", errors.New("cdp: version endpoint: no webSocketDebuggerUrl (not a CDP endpoint)")
	}
	product = v.Product
	if product == "" {
		product = v.Browser
	}
	return v.WebSocketDebuggerURL, product, nil
}

// Dial connects to the browser-level WebSocket of the CDP
// endpoint listening on the given loopback port.
func Dial(ctx context.Context, port int) (*Client, error) {
	wsURL, _, err := FetchVersion(ctx, port)
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp: dial: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	c := &Client{
		conn:    conn,
		done:    make(chan struct{}),
		pending: make(map[int64]*pendingCall),
	}
	c.evCond = sync.NewCond(&c.evMu)
	go c.readLoop()
	go c.eventLoop()
	return c, nil
}

// OnEvent installs the event handler. Events are delivered in
// arrival order from a dedicated goroutine; the handler may call
// back into this client.
func (c *Client) OnEvent(h Handler) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

// Done closes when the connection ends for any reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close ends the connection and unblocks all pending calls.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.conn.CloseNow()
		c.failAll(errors.New("cdp: connection closed"))
	})
}

func (c *Client) failAll(err error) {
	c.mu.Lock()
	pending := c.pending
	c.pending = make(map[int64]*pendingCall)
	c.mu.Unlock()
	for _, p := range pending {
		p.ch <- callResult{err: err}
	}
	c.closeEvents()
	// done must close exactly once even when failAll is entered
	// concurrently (Close racing the read loop's failure path):
	// a check-then-close select here panicked with "close of
	// closed channel" when both sides saw the channel still open.
	c.doneOnce.Do(func() { close(c.done) })
}

// enqueueEvent appends an event for the delivery goroutine. It
// only takes a short lock — the read loop never waits for the
// handler.
func (c *Client) enqueueEvent(ev Event) {
	c.evMu.Lock()
	if !c.evClosed {
		c.evQueue = append(c.evQueue, ev)
		c.evCond.Signal()
	}
	c.evMu.Unlock()
}

func (c *Client) closeEvents() {
	c.evMu.Lock()
	c.evClosed = true
	c.evCond.Broadcast()
	c.evMu.Unlock()
}

// eventLoop delivers queued events to the handler in order. It
// exits when the connection closes, so no goroutine outlives its
// client.
func (c *Client) eventLoop() {
	for {
		c.evMu.Lock()
		for len(c.evQueue) == 0 && !c.evClosed {
			c.evCond.Wait()
		}
		if len(c.evQueue) == 0 && c.evClosed {
			c.evMu.Unlock()
			return
		}
		ev := c.evQueue[0]
		c.evQueue = c.evQueue[1:]
		c.evMu.Unlock()
		c.mu.Lock()
		h := c.handler
		c.mu.Unlock()
		if h != nil {
			h(ev)
		}
	}
}

type wireMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.failAll(fmt.Errorf("cdp: read: %w", err))
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			p := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if p == nil {
				continue
			}
			if msg.Error != nil {
				p.ch <- callResult{err: fmt.Errorf("cdp: %s: %s", "error", msg.Error.Message)}
			} else {
				p.ch <- callResult{result: msg.Result}
			}
			continue
		}
		if msg.Method != "" {
			c.enqueueEvent(Event{Method: msg.Method, SessionID: msg.SessionID, Params: msg.Params})
		}
	}
}

// Call issues one protocol command and waits for its response.
// sessionID may be empty for browser-level commands. The wait is
// bounded by ctx — callers use short per-command contexts so a
// slow or dead peer can never stall other instances' dispatch.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	msg := wireMessage{ID: id, Method: method, SessionID: sessionID, Params: raw}
	payload, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	p := &pendingCall{ch: make(chan callResult, 1)}
	c.mu.Lock()
	select {
	case <-c.done:
		c.mu.Unlock()
		return nil, errors.New("cdp: connection closed")
	default:
	}
	c.pending[id] = p
	c.mu.Unlock()

	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	err = c.conn.Write(writeCtx, websocket.MessageText, payload)
	cancel()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("cdp: write %s: %w", method, err)
	}
	select {
	case res := <-p.ch:
		return res.result, res.err
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("cdp: connection closed")
	}
}

// ---- browser & target helpers -------------------------------------

type Version struct {
	Product string `json:"product"`
}

func (c *Client) BrowserVersion(ctx context.Context) (Version, error) {
	raw, err := c.Call(ctx, "", "Browser.getVersion", nil)
	if err != nil {
		return Version{}, err
	}
	var v Version
	if err := json.Unmarshal(raw, &v); err != nil {
		return Version{}, err
	}
	return v, nil
}

type TargetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
	Title    string `json:"title"`
	Attached bool   `json:"attached"`
}

func (c *Client) Targets(ctx context.Context) ([]TargetInfo, error) {
	raw, err := c.Call(ctx, "", "Target.getTargets", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		TargetInfos []TargetInfo `json:"targetInfos"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.TargetInfos, nil
}

// Attach attaches to a target in flatten mode and returns the
// session id used for page-level commands.
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

func (c *Client) CloseTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.closeTarget", map[string]any{"targetId": targetID})
	return err
}

func (c *Client) ActivateTarget(ctx context.Context, targetID string) error {
	_, err := c.Call(ctx, "", "Target.activateTarget", map[string]any{"targetId": targetID})
	return err
}

// EnableTargetEvents asks the browser to report target lifecycle
// events for all existing and future targets.
func (c *Client) EnableTargetEvents(ctx context.Context) error {
	_, err := c.Call(ctx, "", "Target.setDiscoverTargets", map[string]any{"discover": true})
	return err
}

// ---- page helpers ---------------------------------------------------

// LayoutViewport returns the page's CSS layout viewport size for
// a session — the coordinate space CDP input events use.
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
		CSSLayoutViewport struct {
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, 0, err
	}
	if out.CSSLayoutViewport.ClientWidth > 0 {
		return out.CSSLayoutViewport.ClientWidth, out.CSSLayoutViewport.ClientHeight, nil
	}
	return out.LayoutViewport.ClientWidth, out.LayoutViewport.ClientHeight, nil
}

func (c *Client) Navigate(ctx context.Context, sessionID, url string) error {
	_, err := c.Call(ctx, sessionID, "Page.navigate", map[string]any{"url": url})
	return err
}

func (c *Client) Reload(ctx context.Context, sessionID string) error {
	_, err := c.Call(ctx, sessionID, "Page.reload", nil)
	return err
}

// HistoryEntry is one entry of a page's navigation history.
type HistoryEntry struct {
	ID  int    `json:"id"`
	URL string `json:"url"`
}

// NavigationHistory returns the session page's history stack and
// the index of the current entry.
func (c *Client) NavigationHistory(ctx context.Context, sessionID string) (entries []HistoryEntry, currentIndex int, err error) {
	raw, err := c.Call(ctx, sessionID, "Page.getNavigationHistory", nil)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		CurrentIndex int            `json:"currentIndex"`
		Entries      []HistoryEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, err
	}
	return out.Entries, out.CurrentIndex, nil
}

// NavigateToHistoryEntry moves the page to one of its own
// history entries — the semantic form of back/forward that keeps
// each instance on its own history stack.
func (c *Client) NavigateToHistoryEntry(ctx context.Context, sessionID string, entryID int) error {
	_, err := c.Call(ctx, sessionID, "Page.navigateToHistoryEntry", map[string]any{"entryId": entryID})
	return err
}

// ---- input ----------------------------------------------------------

type MouseEvent struct {
	Type       string // mouseMoved, mousePressed, mouseReleased, mouseWheel
	X, Y       float64
	Button     string // none, left, middle, right
	Buttons    int    // bitmask of currently pressed buttons
	ClickCount int
	DeltaX     float64
	DeltaY     float64
}

// Mouse dispatches one Input.dispatchMouseEvent.
func (c *Client) Mouse(ctx context.Context, sessionID string, ev MouseEvent) error {
	params := map[string]any{
		"type": ev.Type, "x": ev.X, "y": ev.Y,
		"button": ev.Button, "buttons": ev.Buttons, "clickCount": ev.ClickCount,
	}
	if ev.Type == "mouseWheel" {
		params["deltaX"] = ev.DeltaX
		params["deltaY"] = ev.DeltaY
	}
	_, err := c.Call(ctx, sessionID, "Input.dispatchMouseEvent", params)
	return err
}

type KeyEvent struct {
	Type      string // keyDown, keyUp, rawKeyDown, char
	Key       string // e.g. "a", "Enter", "ArrowLeft", "F5"
	Code      string // e.g. "KeyA", "Enter", "ArrowLeft"
	VK        int    // windows virtual key code
	Modifiers int    // Alt=1, Ctrl=2, Meta=4, Shift=8
	Text      string // for printable keys on keyDown/char
}

// Key dispatches one Input.dispatchKeyEvent.
func (c *Client) Key(ctx context.Context, sessionID string, ev KeyEvent) error {
	params := map[string]any{
		"type": ev.Type, "key": ev.Key, "code": ev.Code,
		"windowsVirtualKeyCode": ev.VK, "modifiers": ev.Modifiers,
	}
	if ev.Text != "" {
		params["text"] = ev.Text
	}
	_, err := c.Call(ctx, sessionID, "Input.dispatchKeyEvent", params)
	return err
}
