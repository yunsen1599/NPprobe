// Package panel 测试端与面板之间的两个接口：
//   - GET  <panel>/v1/probe/targets  拉取测试目标和测试参数
//   - POST <panel>/v1/probe/report   回报一轮结果（同时是心跳）
package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ErrUnauthorized token 无效或测试端已停用
var ErrUnauthorized = errors.New("panel rejected token (probe disabled or token reset)")

type Config struct {
	RoundInterval int      `json:"round_interval"`
	Concurrency   int      `json:"concurrency"`
	TimeoutMS     int      `json:"timeout_ms"`
	TestURLs      []string `json:"test_urls"`
}

type Target struct {
	NodeID   int64          `json:"node_id"`
	ServerID int64          `json:"server_id"`
	Name     string         `json:"name"`
	NodeType string         `json:"node_type"`
	Address  string         `json:"address"`
	Port     int64          `json:"port"`
	Protocol string         `json:"protocol"`
	Proxy    map[string]any `json:"proxy"` // 一条 Clash（mihomo）代理配置，直接交给 mihomo
}

type Targets struct {
	ProbeID int64    `json:"probe_id"`
	Name    string   `json:"name"`
	UUID    string   `json:"uuid"`
	Config  Config   `json:"config"`
	Targets []Target `json:"targets"`
}

type Result struct {
	NodeID    int64  `json:"node_id"`
	Status    string `json:"status"` // ok / tcp_fail / proxy_fail
	TCPMS     int64  `json:"tcp_ms"`
	DelayMS   int64  `json:"delay_ms"`
	Error     string `json:"error"`
	CheckedAt int64  `json:"checked_at"`
}

type Report struct {
	Version string   `json:"version"`
	RoundID string   `json:"round_id"`
	Results []Result `json:"results"`
}

type Client struct {
	base  string
	token string
	agent string
	http  *http.Client
}

func New(base, token, version string) *Client {
	return &Client{
		base:  base,
		token: token,
		agent: "NPprobe/" + version,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("User-Agent", c.agent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: http %d: %.200s", method, path, resp.StatusCode, raw)
	}
	if resp.StatusCode != http.StatusOK || env.Code != http.StatusOK {
		return fmt.Errorf("%s %s: http %d code %d: %s", method, path, resp.StatusCode, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		return json.Unmarshal(env.Data, out)
	}
	return nil
}

func (c *Client) Targets(ctx context.Context) (*Targets, error) {
	var out Targets
	if err := c.do(ctx, http.MethodGet, "/v1/probe/targets", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Report(ctx context.Context, r Report) error {
	return c.do(ctx, http.MethodPost, "/v1/probe/report", r, nil)
}
