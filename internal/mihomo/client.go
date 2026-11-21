package mihomo

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Client is a minimal REST client for the Mihomo core's external
// controller. It implements only the endpoints DeBrowser uses.
// The controller secret is sent as a bearer token and never
// logged or included in error messages.
type Client struct {
	base   string
	secret string
	hc     *http.Client
}

// NewClient builds a client for a controller on 127.0.0.1:port.
func NewClient(port int, secret string) *Client {
	return &Client{
		base:   fmt.Sprintf("http://127.0.0.1:%d", port),
		secret: secret,
		hc:     &http.Client{Timeout: 10 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return nil, err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("mihomo api %s %s: status %d", method, path, resp.StatusCode)
	}
	return data, nil
}

// Version returns the core's version string (for example
// "v1.19.34"). It doubles as the readiness probe.
func (c *Client) Version(ctx context.Context) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/version", nil)
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("mihomo api /version: %w", err)
	}
	return v.Version, nil
}

// NodeInfo is one proxy node of a provider, as the node list UI
// needs it. Delay is the node's most recent health-check delay in
// milliseconds (0 when never measured). Checked reports whether
// the core has health-checked the node at all (it has a check
// history); Alive is only meaningful when Checked is true — an
// unchecked node is neither healthy nor failed.
type NodeInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Alive   bool   `json:"alive"`
	Delay   int    `json:"delay"`
	Checked bool   `json:"checked"`
}

// ProviderProxies lists the nodes one proxy-provider currently
// holds. A provider that failed to load (bad subscription payload,
// unreadable file) is reported by the core with an error or an
// empty vehicle; both surface here as an error or an empty list.
func (c *Client) ProviderProxies(ctx context.Context, provider string) ([]NodeInfo, error) {
	data, err := c.do(ctx, http.MethodGet, "/providers/proxies/"+url.PathEscape(provider), nil)
	if err != nil {
		return nil, err
	}
	var p struct {
		Proxies []struct {
			Name    string `json:"name"`
			Type    string `json:"type"`
			Alive   bool   `json:"alive"`
			History []struct {
				Delay int `json:"delay"`
			} `json:"history"`
		} `json:"proxies"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("mihomo api provider proxies: %w", err)
	}
	out := make([]NodeInfo, 0, len(p.Proxies))
	for _, n := range p.Proxies {
		ni := NodeInfo{Name: n.Name, Type: n.Type, Alive: n.Alive, Checked: len(n.History) > 0}
		if len(n.History) > 0 {
			ni.Delay = n.History[len(n.History)-1].Delay
		}
		out = append(out, ni)
	}
	return out, nil
}

// RefreshProvider asks the core to re-fetch one proxy-provider
// (a subscription update). The core keeps the previous nodes when
// the fetch fails.
func (c *Client) RefreshProvider(ctx context.Context, provider string) error {
	_, err := c.do(ctx, http.MethodPut, "/providers/proxies/"+url.PathEscape(provider), nil)
	return err
}

// GroupDelay runs a delay test for every member of a group and
// returns node name -> delay in milliseconds. Names that failed
// the test are absent from the map.
func (c *Client) GroupDelay(ctx context.Context, group string, timeoutMs int) (map[string]int, error) {
	q := url.Values{}
	q.Set("url", "https://www.gstatic.com/generate_204")
	q.Set("timeout", strconv.Itoa(timeoutMs))
	data, err := c.do(ctx, http.MethodGet, "/group/"+url.PathEscape(group)+"/delay?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("mihomo api group delay: %w", err)
	}
	return out, nil
}

// SelectNode sets a select group's current node. Selections are
// applied by DeBrowser from its own binding records after every
// core start and configuration reload; the core is never the
// record of truth for them.
func (c *Client) SelectNode(ctx context.Context, group, node string) error {
	_, err := c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), map[string]string{"name": node})
	return err
}

// ReloadConfig hot-reloads the core from a configuration file
// path (PUT /configs). A payload the core rejects leaves the
// previously running configuration untouched.
func (c *Client) ReloadConfig(ctx context.Context, configPath string) error {
	_, err := c.do(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": configPath})
	return err
}
