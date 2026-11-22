package mihomo

import (
	"bufio"
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
// the test are absent from the map. testURL and timeoutMs come
// from the editable core settings.
func (c *Client) GroupDelay(ctx context.Context, group, testURL string, timeoutMs int) (map[string]int, error) {
	q := url.Values{}
	q.Set("url", testURL)
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

// GroupNow returns a proxy group's currently selected member
// (the "now" field of the core's per-proxy view). For an instance
// group that is the bound node's name, or REJECT while the bound
// node is unavailable.
func (c *Client) GroupNow(ctx context.Context, group string) (string, error) {
	data, err := c.do(ctx, http.MethodGet, "/proxies/"+url.PathEscape(group), nil)
	if err != nil {
		return "", err
	}
	var g struct {
		Now string `json:"now"`
	}
	if err := json.Unmarshal(data, &g); err != nil {
		return "", fmt.Errorf("mihomo api group now: %w", err)
	}
	return g.Now, nil
}

// ReloadConfig hot-reloads the core from a configuration file
// path (PUT /configs). A payload the core rejects leaves the
// previously running configuration untouched.
func (c *Client) ReloadConfig(ctx context.Context, configPath string) error {
	_, err := c.do(ctx, http.MethodPut, "/configs?force=true", map[string]string{"path": configPath})
	return err
}

// RefreshRuleProvider asks the core to re-fetch one rule-provider
// (the "update rulesets now" action). The core answers 503 when
// the fetch itself fails and keeps serving the cached copy; the
// error surfaces that.
func (c *Client) RefreshRuleProvider(ctx context.Context, provider string) error {
	_, err := c.do(ctx, http.MethodPut, "/providers/rules/"+url.PathEscape(provider), nil)
	return err
}

// ConnectionInfo is one active connection of the core, as the
// connections view needs it. InboundPort / InboundName identify
// the listener the connection entered through — for DeBrowser
// configurations that is exactly one instance's listener, which
// is what makes per-instance filtering exact.
type ConnectionInfo struct {
	ID              string
	Host            string // requested host (sniffed host when present)
	DestinationIP   string
	DestinationPort string
	InboundPort     int
	InboundName     string
	Rule            string // e.g. RuleSet, GeoIP, Match, DomainSuffix
	RulePayload     string // e.g. the matched ruleset's name
	Chains          []string
	Upload          int64
	Download        int64
	Start           string
}

// Connections returns the core's current active connections
// (GET /connections snapshot).
func (c *Client) Connections(ctx context.Context) ([]ConnectionInfo, error) {
	data, err := c.do(ctx, http.MethodGet, "/connections", nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Connections []struct {
			ID       string `json:"id"`
			Metadata struct {
				Host            string `json:"host"`
				SniffHost       string `json:"sniffHost"`
				DestinationIP   string `json:"destinationIP"`
				DestinationPort string `json:"destinationPort"`
				InboundPort     string `json:"inboundPort"`
				InboundName     string `json:"inboundName"`
			} `json:"metadata"`
			Upload      int64    `json:"upload"`
			Download    int64    `json:"download"`
			Start       string   `json:"start"`
			Chains      []string `json:"chains"`
			Rule        string   `json:"rule"`
			RulePayload string   `json:"rulePayload"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mihomo api connections: %w", err)
	}
	out := make([]ConnectionInfo, 0, len(raw.Connections))
	for _, rc := range raw.Connections {
		host := rc.Metadata.Host
		if host == "" {
			host = rc.Metadata.SniffHost
		}
		port, _ := strconv.Atoi(rc.Metadata.InboundPort)
		out = append(out, ConnectionInfo{
			ID: rc.ID, Host: host,
			DestinationIP: rc.Metadata.DestinationIP, DestinationPort: rc.Metadata.DestinationPort,
			InboundPort: port, InboundName: rc.Metadata.InboundName,
			Rule: rc.Rule, RulePayload: rc.RulePayload, Chains: rc.Chains,
			Upload: rc.Upload, Download: rc.Download, Start: rc.Start,
		})
	}
	return out, nil
}

// LogLineHandler receives one core log line (level, message).
type LogLineHandler func(level, payload string)

// StreamLogs forwards the core's log stream (GET /logs) line by
// line until ctx ends or the stream breaks. Lines are
// newline-delimited JSON objects {"type": "...", "payload":
// "..."}; unparsable lines are skipped, never fatal. The
// request deliberately uses a client WITHOUT the usual 10s
// timeout — the stream is meant to stay open.
func (c *Client) StreamLogs(ctx context.Context, level string, fn LogLineHandler) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/logs?level="+url.QueryEscape(level), nil)
	if err != nil {
		return err
	}
	if c.secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.secret)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("mihomo api GET /logs: status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var line struct {
			Type    string `json:"type"`
			Payload string `json:"payload"`
		}
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue
		}
		fn(line.Type, line.Payload)
	}
	return sc.Err()
}
