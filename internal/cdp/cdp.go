// Package cdp is the minimal Chrome DevTools Protocol client
// DeBrowser's multi-instance synchronization (Stage 10) needs:
// one managed WebSocket connection per browser instance,
// command/response correlation with timeouts, session support
// and event delivery. It implements nothing else.
//
// Safety rules enforced here:
//   - Discovery and dialing only ever talk to loopback
//     addresses, and the discovered WebSocket endpoint must
//     belong to the exact port the instance was assigned.
//   - No call waits forever: every command carries the caller's
//     context.
//   - Event handlers never run on the read loop and never under
//     the client lock, so a handler may itself issue commands.
//   - Raw protocol payloads are never logged by this package.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Discover queries http://127.0.0.1:<port>/json/version and
// returns the browser WebSocket URL after verifying that it is
// a loopback address on exactly the expected port. Redirects
// are refused: a debugging endpoint never legitimately
// redirects, and following one could leave loopback.
func Discover(ctx context.Context, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("调试端口无效")
	}
	hc := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接浏览器调试端口失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("浏览器调试端口返回异常状态：%s", resp.Status)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&version); err != nil {
		return "", fmt.Errorf("浏览器调试端口响应无法解析：%w", err)
	}
	if err := ValidateLoopbackWSURL(version.WebSocketDebuggerURL, port); err != nil {
		return "", err
	}
	return version.WebSocketDebuggerURL, nil
}

// ValidateLoopbackWSURL reports whether raw is a ws/wss URL on
// a loopback host; when wantPort > 0 the URL must carry exactly
// that port.
func ValidateLoopbackWSURL(raw string, wantPort int) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") {
		return fmt.Errorf("浏览器调试地址无效")
	}
	host := u.Hostname()
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("浏览器调试地址不是本机回环地址")
	}
	if wantPort > 0 && u.Port() != fmt.Sprintf("%d", wantPort) {
		return fmt.Errorf("浏览器调试地址端口与实例分配不一致")
	}
	return nil
}

// Event is one protocol event. Params is the raw params object;
// handlers decode only the fields they need.
type Event struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// EventHandler consumes one event. It runs on its own
// goroutine, never on the read loop.
type EventHandler func(Event)

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

type pendingCall struct {
	ch chan wireMessage
}

// Client is one browser's CDP connection.
type Client struct {
	conn *websocket.Conn

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]*pendingCall
	closed  bool

	handlersMu sync.RWMutex
	handlers   map[string][]EventHandler

	done     chan struct{}
	doneOnce sync.Once
}

// Dial connects to a previously discovered WebSocket URL. The
// URL is re-validated here so no caller can dial a remote
// endpoint by mistake.
func Dial(ctx context.Context, wsURL string) (*Client, error) {
	if err := ValidateLoopbackWSURL(wsURL, 0); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接浏览器调试通道失败：%w", err)
	}
	conn.SetReadLimit(64 << 20)
	c := &Client{
		conn:     conn,
		pending:  map[int64]*pendingCall{},
		handlers: map[string][]EventHandler{},
		done:     make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Done is closed when the connection ends for any reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// On registers a handler for one event method. Handlers run on
// fresh goroutines; ordering between events is not guaranteed
// at this layer (the sync engine serializes per target).
func (c *Client) On(method string, h EventHandler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[method] = append(c.handlers[method], h)
}

func (c *Client) readLoop() {
	defer c.shutdown()
	for {
		var msg wireMessage
		if err := wsjson.Read(context.Background(), c.conn, &msg); err != nil {
			return
		}
		if msg.ID != 0 {
			c.mu.Lock()
			p := c.pending[msg.ID]
			delete(c.pending, msg.ID)
			c.mu.Unlock()
			if p != nil {
				p.ch <- msg
			}
			continue
		}
		if msg.Method == "" {
			continue
		}
		c.handlersMu.RLock()
		hs := append([]EventHandler(nil), c.handlers[msg.Method]...)
		c.handlersMu.RUnlock()
		ev := Event{Method: msg.Method, SessionID: msg.SessionID, Params: msg.Params}
		for _, h := range hs {
			go h(ev)
		}
	}
}

func (c *Client) shutdown() {
	c.doneOnce.Do(func() {
		close(c.done)
		c.mu.Lock()
		c.closed = true
		pending := c.pending
		c.pending = map[int64]*pendingCall{}
		c.mu.Unlock()
		for _, p := range pending {
			p.ch <- wireMessage{Error: &struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}{Code: -1, Message: "连接已关闭"}}
		}
		_ = c.conn.Close(websocket.StatusNormalClosure, "")
	})
}

// Close ends the connection and fails every pending call.
func (c *Client) Close() { c.shutdown() }

var errClosed = errors.New("调试连接已关闭")

// Call sends one command and waits for its response or the
// context. sessionID may be empty for browser-level commands.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}
	id := c.nextID.Add(1)
	p := &pendingCall{ch: make(chan wireMessage, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	c.pending[id] = p
	c.mu.Unlock()

	msg := wireMessage{ID: id, Method: method, SessionID: sessionID, Params: rawParams}
	if err := wsjson.Write(ctx, c.conn, msg); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	select {
	case resp := <-p.ch:
		if resp.Error != nil {
			return nil, fmt.Errorf("调试命令 %s 失败（%d）：%s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, errClosed
	}
}

// CallString is Call for handlers that need one string field of
// the result (e.g. Target.createTarget's targetId).
func (c *Client) CallString(ctx context.Context, sessionID, method string, params any, field string) (string, error) {
	raw, err := c.Call(ctx, sessionID, method, params)
	if err != nil {
		return "", err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	var v string
	if err := json.Unmarshal(out[field], &v); err != nil {
		return "", fmt.Errorf("调试命令 %s 的响应缺少字段 %s", method, field)
	}
	return v, nil
}
