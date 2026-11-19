// Package cdp is the minimal Chrome DevTools Protocol client used
// by Stage 10 multi-instance synchronization. It implements only
// what the sync feature needs: one WebSocket connection per browser
// instance, command/response correlation by ID, and event delivery
// to a single handler. Command sending and event reading are
// separate: the read loop never executes protocol logic, it only
// completes pending calls and forwards events.
//
// Every address this package touches must be a loopback address:
// the debugging port belongs to a browser instance on this machine
// and must never be reachable from — or redirected to — the
// network.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Event is one protocol event delivered by the browser.
type Event struct {
	SessionID string
	Method    string
	Params    json.RawMessage
}

// Handler receives protocol events. It is called from the client's
// read goroutine and must not block for long; implementations hand
// events to their own queues.
type Handler func(Event)

type callResult struct {
	result json.RawMessage
	err    error
}

type wireMessage struct {
	ID        int64           `json:"id,omitempty"`
	Method    string          `json:"method,omitempty"`
	Params    json.RawMessage `json:"params,omitempty"`
	SessionID string          `json:"sessionId,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Client is one CDP connection to one browser instance.
type Client struct {
	conn    *websocket.Conn
	handler Handler

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan callResult
	closed  bool

	done     chan struct{}
	doneOnce sync.Once
	failErr  error
}

// ValidateLoopbackWSURL checks that raw is a WebSocket URL pointing
// at a loopback host and — when wantPort > 0 — exactly that port.
// CDP endpoints come from the browser's own discovery document, but
// they are still verified before dialing: a debugging endpoint must
// never point anywhere else.
func ValidateLoopbackWSURL(raw string, wantPort int) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("调试地址无效：%w", err)
	}
	if u.Scheme != "ws" {
		return fmt.Errorf("调试地址协议不受支持：%s", u.Scheme)
	}
	host := u.Hostname()
	if !isLoopbackHost(host) {
		return fmt.Errorf("调试地址不是本机回环地址：%s", host)
	}
	if wantPort > 0 {
		port, perr := strconv.Atoi(u.Port())
		if perr != nil || port != wantPort {
			return fmt.Errorf("调试地址端口与实例端口不一致")
		}
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Discover queries the browser's HTTP discovery endpoint on the
// given loopback port and returns its validated WebSocket URL.
// Redirects are refused: the discovery document must come from the
// local browser itself.
func Discover(ctx context.Context, port int) (string, error) {
	if port <= 0 || port > 65535 {
		return "", fmt.Errorf("调试端口无效：%d", port)
	}
	hc := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("调试地址不允许重定向")
		},
	}
	req, err := http.NewRequestWithContext(ctx,
		http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/json/version", port), nil)
	if err != nil {
		return "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接浏览器调试端口失败：%w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("浏览器调试端口返回状态 %d", resp.StatusCode)
	}
	var doc struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		return "", fmt.Errorf("解析浏览器调试信息失败：%w", err)
	}
	if doc.WebSocketDebuggerURL == "" {
		return "", errors.New("浏览器未提供调试 WebSocket 地址")
	}
	if err := ValidateLoopbackWSURL(doc.WebSocketDebuggerURL, port); err != nil {
		return "", err
	}
	return doc.WebSocketDebuggerURL, nil
}

// Dial opens a CDP connection to an already validated WebSocket
// URL. The handler receives every protocol event.
func Dial(ctx context.Context, wsURL string, handler Handler) (*Client, error) {
	if err := ValidateLoopbackWSURL(wsURL, 0); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("连接浏览器调试通道失败：%w", err)
	}
	conn.SetReadLimit(16 << 20)
	c := &Client{
		conn:    conn,
		handler: handler,
		pending: make(map[int64]chan callResult),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Done is closed when the connection ends for any reason.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the failure that ended the connection, if any.
func (c *Client) Err() error {
	select {
	case <-c.done:
		return c.failErr
	default:
		return nil
	}
}

// Call sends one command (optionally on a flattened session) and
// waits for its response.
func (c *Client) Call(ctx context.Context, sessionID, method string, params any) (json.RawMessage, error) {
	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("调试连接已关闭")
	}
	c.nextID++
	id := c.nextID
	ch := make(chan callResult, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	msg, err := json.Marshal(wireMessage{ID: id, Method: method, Params: rawParams, SessionID: sessionID})
	if err != nil {
		c.removePending(id)
		return nil, err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, msg); err != nil {
		c.removePending(id)
		return nil, fmt.Errorf("发送调试命令失败：%w", err)
	}
	select {
	case res := <-ch:
		return res.result, res.err
	case <-ctx.Done():
		c.removePending(id)
		return nil, ctx.Err()
	case <-c.done:
		if c.failErr != nil {
			return nil, c.failErr
		}
		return nil, errors.New("调试连接已关闭")
	}
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// Close ends the connection and fails every pending call.
func (c *Client) Close() error {
	err := c.conn.Close(websocket.StatusNormalClosure, "")
	c.failAll(errors.New("调试连接已关闭"))
	return err
}

func (c *Client) failAll(err error) {
	c.doneOnce.Do(func() {
		c.failErr = err
		close(c.done)
	})
	c.mu.Lock()
	c.closed = true
	pending := c.pending
	c.pending = make(map[int64]chan callResult)
	c.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- callResult{err: err}:
		default:
		}
	}
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.Read(context.Background())
		if err != nil {
			c.failAll(fmt.Errorf("调试连接中断：%w", err))
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if msg.ID != 0 {
			c.mu.Lock()
			ch, ok := c.pending[msg.ID]
			if ok {
				delete(c.pending, msg.ID)
			}
			c.mu.Unlock()
			if !ok {
				continue
			}
			res := callResult{result: msg.Result}
			if msg.Error != nil {
				res.err = fmt.Errorf("调试命令失败（%s）：%s", msg.Method, msg.Error.Message)
				if msg.Method == "" {
					res.err = fmt.Errorf("调试命令失败：%s", msg.Error.Message)
				}
			}
			select {
			case ch <- res:
			default:
			}
			continue
		}
		if msg.Method != "" && c.handler != nil {
			c.handler(Event{SessionID: msg.SessionID, Method: msg.Method, Params: msg.Params})
		}
	}
}
