package tester

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

// Downloader 经代理下载，返回下载的字节数和耗时（mihomo 代理实现了它，便于测试替换）
type Downloader interface {
	Download(ctx context.Context, url string, maxBytes int64) (int64, time.Duration, error)
}

// Download 经 mihomo 代理建立连接，用普通的 HTTP 客户端下载（TLS 由 HTTP 客户端处理）
func (m mihomoTester) Download(ctx context.Context, url string, maxBytes int64) (int64, time.Duration, error) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			var md C.Metadata
			if err := md.SetRemoteAddress(addr); err != nil {
				return nil, err
			}
			md.NetWork = C.TCP
			return m.p.DialContext(ctx, &md)
		},
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		DisableKeepAlives:   true,
	}
	defer transport.CloseIdleConnections()
	return download(ctx, &http.Client{Transport: transport}, url, maxBytes)
}

func download(ctx context.Context, client *http.Client, url string, maxBytes int64) (int64, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, errors.New(resp.Status)
	}
	n, err := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBytes))
	elapsed := time.Since(start)
	if err != nil && n == 0 {
		return 0, elapsed, err
	}
	return n, elapsed, nil
}

// Kbps 下载速度（千比特每秒）；数据太少（< 64KB）时不可信，返回 0
func Kbps(bytes int64, d time.Duration) int64 {
	if bytes < 64<<10 || d <= 0 {
		return 0
	}
	return int64(float64(bytes*8) / d.Seconds() / 1000)
}

// SpeedDue 这个节点本轮是否测速：按节点 ID 错开，每 every 轮一次
func SpeedDue(nodeID int64, round int64, every int) bool {
	if every <= 0 {
		return false
	}
	return (nodeID+round)%int64(every) == 0
}
