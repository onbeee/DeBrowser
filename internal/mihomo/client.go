package mihomo

// A minimal client for the core's control API — only what the
// proxy module uses: proving the core answers (and that it is
// the core DeBrowser started, via the shared secret) and
// asking it to reload its configuration after a change. Errors
// carry only the method, path and status: never the secret and
// never a URL.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	base   string
	secret string
	http   *http.Client
}

func NewClient(port int, secret string) *Client {
	return &Client{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		secret: secret,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string) (int, error) {
	return c.doBody(ctx, method, path, nil)
}

func (c *Client) doBody(ctx context.Context, method, path string, body io.Reader) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// Version reports whether the core answers its version
// endpoint. With the secret configured, a 200 also proves the
// answering process holds the same secret — i.e. it is the
// core DeBrowser started, not a foreign program.
func (c *Client) Version(ctx context.Context) error {
	status, err := c.do(ctx, http.MethodGet, "/version")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("GET /version: status %d", status)
	}
	return nil
}

// ReloadConfig asks the running core to load the given
// configuration file (used after bindings or nodes change).
// The endpoint requires the path in a JSON body — an empty
// body is a 400.
func (c *Client) ReloadConfig(ctx context.Context, configPath string) error {
	payload, err := json.Marshal(map[string]string{"path": configPath})
	if err != nil {
		return err
	}
	status, err := c.doBody(ctx, http.MethodPut, "/configs?force=true", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusOK {
		return fmt.Errorf("PUT /configs: status %d", status)
	}
	return nil
}
