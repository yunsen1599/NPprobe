package runner

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yunsen1599/NPprobe/internal/panel"
	"github.com/yunsen1599/NPprobe/internal/tester"
)

type fakePanel struct {
	mu      sync.Mutex
	targets *panel.Targets
	reports []panel.Report
	failing bool
}

func (f *fakePanel) Targets(context.Context) (*panel.Targets, error) { return f.targets, nil }
func (f *fakePanel) Report(_ context.Context, r panel.Report) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing {
		return errors.New("panel down")
	}
	f.reports = append(f.reports, r)
	return nil
}

type okProxy struct{}

func (okProxy) URLTest(context.Context, string, []uint16) (uint16, error) { return 50, nil }

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestApplyTargetsRebuildsOnlyChanged(t *testing.T) {
	builds := 0
	closed := 0
	r := &Runner{Log: quiet(), Factory: func(map[string]any) (tester.URLTester, func(), error) {
		builds++
		return okProxy{}, func() { closed++ }, nil
	}}
	t1 := panel.Target{NodeID: 1, Proxy: map[string]any{"type": "vless", "uuid": "a"}}
	t2 := panel.Target{NodeID: 2, Proxy: map[string]any{"type": "vless", "uuid": "b"}}
	r.applyTargets(&panel.Targets{Targets: []panel.Target{t1, t2}})
	if builds != 2 {
		t.Fatalf("builds=%d", builds)
	}
	t2.Proxy = map[string]any{"type": "vless", "uuid": "c"}
	r.applyTargets(&panel.Targets{Targets: []panel.Target{t1, t2}})
	if builds != 3 || closed != 1 {
		t.Fatalf("only the changed target is rebuilt: builds=%d closed=%d", builds, closed)
	}
	r.applyTargets(&panel.Targets{Targets: []panel.Target{t1}})
	if closed != 2 || len(r.entries) != 1 {
		t.Fatalf("removed target must be closed: closed=%d entries=%d", closed, len(r.entries))
	}
}

func TestRoundReportsAndBackfills(t *testing.T) {
	p := &fakePanel{targets: &panel.Targets{Config: panel.Config{Concurrency: 2}, Targets: []panel.Target{
		{NodeID: 1, Protocol: "hysteria2"}, {NodeID: 2, Protocol: "tuic"},
	}}}
	r := &Runner{Panel: p, Log: quiet(), Version: "t",
		Tester:  &tester.Tester{URLs: []string{"u"}, Timeout: time.Second},
		Factory: func(map[string]any) (tester.URLTester, func(), error) { return okProxy{}, nil, nil }}
	r.applyTargets(p.targets)

	p.failing = true
	r.RunRound(context.Background())
	r.RunRound(context.Background())
	if r.Pending() != 2 {
		t.Fatalf("failed rounds must be kept: pending=%d", r.Pending())
	}
	p.failing = false
	r.RunRound(context.Background())
	if r.Pending() != 0 || len(p.reports) != 3 {
		t.Fatalf("backfill: pending=%d reports=%d", r.Pending(), len(p.reports))
	}
	rep := p.reports[2]
	if len(rep.Results) != 2 || rep.Results[0].Status != tester.StatusOK || rep.Version != "t" || rep.RoundID == "" {
		t.Fatalf("report = %+v", rep)
	}
	if p.reports[0].RoundID == p.reports[1].RoundID {
		t.Fatal("each round needs its own round_id")
	}
}

func TestInvalidProxyConfigReportsProxyFail(t *testing.T) {
	p := &fakePanel{targets: &panel.Targets{Targets: []panel.Target{{NodeID: 7, Proxy: map[string]any{"type": "nope"}}}}}
	r := &Runner{Panel: p, Log: quiet(), Factory: MihomoFactory}
	r.applyTargets(p.targets)
	r.RunRound(context.Background())
	if len(p.reports) != 1 || p.reports[0].Results[0].Status != tester.StatusProxyFail {
		t.Fatalf("reports = %+v", p.reports)
	}
}

// 真实走 mihomo：http 代理（本地 CONNECT 代理）→ 本地 204 服务
func TestMihomoEndToEnd(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/down" {
			_, _ = w.Write(make([]byte, 512<<10))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go serveConnectProxy(ln)
	port := ln.Addr().(*net.TCPAddr).Port

	p := &fakePanel{targets: &panel.Targets{
		Config: panel.Config{TestURLs: []string{target.URL + "/generate_204"}, TimeoutMS: 3000,
			SpeedTest: panel.SpeedTest{EveryRounds: 1, URL: target.URL + "/down", MaxBytes: 256 << 10}},
		Targets: []panel.Target{{NodeID: 1, Name: "local", Protocol: "http", Address: "127.0.0.1", Port: int64(port),
			Proxy: map[string]any{"name": "local", "type": "http", "server": "127.0.0.1", "port": port}}},
	}}
	r := &Runner{Panel: p, Log: quiet(), Factory: MihomoFactory}
	r.applyTargets(p.targets)
	r.RunRound(context.Background())
	if len(p.reports) != 1 {
		t.Fatal("no report")
	}
	res := p.reports[0].Results[0]
	if res.Status != tester.StatusOK || res.SpeedKbps <= 0 {
		t.Fatalf("result must be ok with a speed test through mihomo: %+v", res)
	}

	// 代理关掉后应判为失败
	ln.Close()
	r.RunRound(context.Background())
	if res := p.reports[1].Results[0]; res.Status == tester.StatusOK {
		t.Fatalf("closed proxy must fail: %+v", res)
	}
}

func serveConnectProxy(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer c.Close()
			req, err := http.ReadRequest(bufio.NewReader(c))
			if err != nil || req.Method != http.MethodConnect {
				return
			}
			up, err := net.Dial("tcp", req.Host)
			if err != nil {
				return
			}
			defer up.Close()
			_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
			go func() { _, _ = io.Copy(up, c) }()
			_, _ = io.Copy(c, up)
		}(c)
	}
}

func TestPendingSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	p := &fakePanel{failing: true, targets: &panel.Targets{Targets: []panel.Target{{NodeID: 1, Protocol: "tuic"}}}}
	mk := func() *Runner {
		r := &Runner{Panel: p, Log: quiet(), StateDir: dir,
			Tester:  &tester.Tester{URLs: []string{"u"}, Timeout: time.Second},
			Factory: func(map[string]any) (tester.URLTester, func(), error) { return okProxy{}, nil, nil }}
		r.applyTargets(p.targets)
		return r
	}
	r := mk()
	r.RunRound(context.Background())
	if r.Pending() != 1 {
		t.Fatalf("pending = %d", r.Pending())
	}
	// 新进程：从磁盘读回，面板恢复后一起补报
	r2 := mk()
	r2.loadPending()
	p.failing = false
	r2.RunRound(context.Background())
	if len(p.reports) != 2 || r2.Pending() != 0 {
		t.Fatalf("reports=%d pending=%d", len(p.reports), r2.Pending())
	}
	if _, err := os.Stat(filepath.Join(dir, "pending.json")); !os.IsNotExist(err) {
		t.Fatal("pending file must be removed once everything is reported")
	}
}
