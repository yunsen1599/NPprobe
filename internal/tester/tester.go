// Package tester 对单个节点做一次测试：
//
//	① TCP 连通：直连 address:port 做 TCP 握手（hysteria2 / tuic 等 UDP 协议跳过）
//	② 代理请求：经 mihomo 代理请求测试地址（generate_204），第一个失败再试下一个
//	③ 重试：② 失败时隔 2 秒再试一次，两次都失败才算失败
package tester

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/yunsen1599/NPprobe/internal/panel"
)

const (
	StatusOK        = "ok"
	StatusTCPFail   = "tcp_fail"
	StatusProxyFail = "proxy_fail"

	tcpTimeout = 5 * time.Second
	retryDelay = 2 * time.Second
	maxError   = 256
)

// URLTester 便于测试替换（mihomo 的 C.Proxy 实现了它）
type URLTester interface {
	URLTest(ctx context.Context, url string, expectedStatus []uint16) (uint16, error)
}

// mihomoTester 把 mihomo 的 URLTest（第三个参数是 utils.IntRanges）适配成 URLTester
type mihomoTester struct{ p C.Proxy }

func (m mihomoTester) URLTest(ctx context.Context, url string, _ []uint16) (uint16, error) {
	return m.p.URLTest(ctx, url, nil)
}

// FromProxy 包装 mihomo 代理
func FromProxy(p C.Proxy) URLTester { return mihomoTester{p} }

// IsUDP 跑在 UDP / QUIC 上的协议没有 TCP 握手可测
func IsUDP(protocol string) bool {
	switch strings.ToLower(protocol) {
	case "hysteria2", "hysteria", "tuic", "wireguard":
		return true
	}
	return false
}

// Dialer 便于测试替换
type Dialer func(ctx context.Context, network, address string) (net.Conn, error)

type Tester struct {
	URLs    []string
	Timeout time.Duration
	Dial    Dialer
	Sleep   func(time.Duration)
}

// Test 测试一个目标，返回结果（checked_at 为开始测试的时间）
func (t *Tester) Test(ctx context.Context, target panel.Target, proxy URLTester) panel.Result {
	r := panel.Result{NodeID: target.NodeID, CheckedAt: time.Now().Unix()}

	if !IsUDP(target.Protocol) && target.Address != "" && target.Port > 0 {
		start := time.Now()
		dctx, cancel := context.WithTimeout(ctx, tcpTimeout)
		conn, err := t.dial()(dctx, "tcp", net.JoinHostPort(target.Address, strconv.FormatInt(target.Port, 10)))
		cancel()
		if err != nil {
			r.Status, r.Error = StatusTCPFail, truncate(err.Error())
			return r
		}
		_ = conn.Close()
		r.TCPMS = time.Since(start).Milliseconds()
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			t.sleep()(retryDelay)
		}
		for _, u := range t.URLs {
			cctx, cancel := context.WithTimeout(ctx, t.Timeout)
			delay, err := proxy.URLTest(cctx, u, nil)
			cancel()
			if err == nil {
				r.Status, r.DelayMS = StatusOK, int64(delay)
				return r
			}
			lastErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	r.Status = StatusProxyFail
	if lastErr != nil {
		r.Error = truncate(lastErr.Error())
	} else {
		r.Error = "no test url"
	}
	return r
}

func (t *Tester) dial() Dialer {
	if t.Dial != nil {
		return t.Dial
	}
	var d net.Dialer
	return d.DialContext
}

func (t *Tester) sleep() func(time.Duration) {
	if t.Sleep != nil {
		return t.Sleep
	}
	return time.Sleep
}

func truncate(s string) string {
	if len(s) <= maxError {
		return s
	}
	return strings.ToValidUTF8(s[:maxError], "")
}

// Describe 便于日志
func Describe(t panel.Target) string {
	return fmt.Sprintf("#%d %s (%s %s:%d)", t.NodeID, t.Name, t.Protocol, t.Address, t.Port)
}
