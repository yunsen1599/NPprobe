// Package runner 测试端主循环：
//
//  1. 每 10 分钟拉一次测试目标；某个目标的代理配置变了就重建它的 mihomo 代理对象
//  2. 每轮（面板下发的间隔 ± 30 秒随机抖动）按并发数测试全部目标
//  3. 一轮结束后批量回报；失败的轮次缓存在内存里，下次一起补报（最多保留 1 小时）
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math/big"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
	"github.com/yunsen1599/NPprobe/internal/panel"
	"github.com/yunsen1599/NPprobe/internal/tester"
)

const (
	targetsInterval  = 10 * time.Minute
	jitter           = 30 * time.Second
	pendingMaxAge    = time.Hour
	unauthorizedWait = 5 * time.Minute
	defaultRound     = 300
	defaultConc      = 8
	defaultTimeoutMS = 8000
)

// Panel 面板接口（便于测试替换）
type Panel interface {
	Targets(ctx context.Context) (*panel.Targets, error)
	Report(ctx context.Context, r panel.Report) error
}

// ProxyFactory 由 Clash 代理配置建代理对象（默认用 mihomo）
type ProxyFactory func(mapping map[string]any) (tester.URLTester, func(), error)

// MihomoFactory 用 mihomo 解析 Clash 代理配置
func MihomoFactory(mapping map[string]any) (tester.URLTester, func(), error) {
	p, err := adapter.ParseProxy(mapping)
	if err != nil {
		return nil, nil, err
	}
	return tester.FromProxy(p), func() { _ = closeProxy(p) }, nil
}

func closeProxy(p C.Proxy) error { return p.Close() }

type entry struct {
	target panel.Target
	hash   string
	proxy  tester.URLTester
	close  func()
	err    error // 配置解析失败：每轮直接回报 proxy_fail
}

type Runner struct {
	Panel   Panel
	Version string
	Factory ProxyFactory
	Log     *slog.Logger
	// Tester 可替换（测试用）；为空时按面板参数创建
	Tester *tester.Tester

	mu          sync.Mutex
	cfg         panel.Config
	entries     map[int64]*entry
	order       []int64
	lastTargets time.Time
	pending     []panel.Report
}

// Run 一直运行到 ctx 取消
func (r *Runner) Run(ctx context.Context) error {
	if r.Factory == nil {
		r.Factory = MihomoFactory
	}
	if r.Log == nil {
		r.Log = slog.Default()
	}
	r.entries = map[int64]*entry{}
	// 首次必须拿到测试目标
	for {
		err := r.refreshTargets(ctx)
		if err == nil {
			break
		}
		wait := time.Minute
		if errors.Is(err, panel.ErrUnauthorized) {
			wait = unauthorizedWait
		}
		r.Log.Error("fetch targets failed", "err", err, "retry_in", wait)
		if !sleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
	defer r.closeAll()
	for {
		if time.Since(r.lastTargets) >= targetsInterval {
			if err := r.refreshTargets(ctx); err != nil {
				r.Log.Warn("refresh targets failed, keep previous targets", "err", err)
				if errors.Is(err, panel.ErrUnauthorized) {
					r.dropAll()
				}
			}
		}
		r.RunRound(ctx)
		if !sleepCtx(ctx, r.nextWait()) {
			return ctx.Err()
		}
	}
}

func (r *Runner) nextWait() time.Duration {
	r.mu.Lock()
	sec := r.cfg.RoundInterval
	r.mu.Unlock()
	if sec <= 0 {
		sec = defaultRound
	}
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(2*jitter)))
	return time.Duration(sec)*time.Second - jitter + time.Duration(n.Int64())
}

func (r *Runner) refreshTargets(ctx context.Context) error {
	t, err := r.Panel.Targets(ctx)
	if err != nil {
		return err
	}
	r.applyTargets(t)
	r.Log.Info("targets updated", "probe", t.Name, "count", len(t.Targets),
		"interval", r.cfg.RoundInterval, "concurrency", r.cfg.Concurrency)
	return nil
}

// applyTargets 只重建配置有变化的代理对象，删掉已不在范围内的
func (r *Runner) applyTargets(t *panel.Targets) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[int64]*entry{}
	}
	r.cfg = t.Config
	r.lastTargets = time.Now()
	seen := map[int64]bool{}
	r.order = r.order[:0]
	for _, tg := range t.Targets {
		if tg.NodeID <= 0 || seen[tg.NodeID] {
			continue
		}
		seen[tg.NodeID] = true
		r.order = append(r.order, tg.NodeID)
		raw, _ := json.Marshal(tg)
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if old, ok := r.entries[tg.NodeID]; ok && old.hash == hash {
			continue
		}
		if old, ok := r.entries[tg.NodeID]; ok && old.close != nil {
			old.close()
		}
		e := &entry{target: tg, hash: hash}
		e.proxy, e.close, e.err = r.Factory(tg.Proxy)
		if e.err != nil {
			r.Log.Warn("parse proxy config failed", "node", tester.Describe(tg), "err", e.err)
		}
		r.entries[tg.NodeID] = e
	}
	for id, e := range r.entries {
		if !seen[id] {
			if e.close != nil {
				e.close()
			}
			delete(r.entries, id)
		}
	}
}

func (r *Runner) dropAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, e := range r.entries {
		if e.close != nil {
			e.close()
		}
		delete(r.entries, id)
	}
	r.order = nil
}

func (r *Runner) closeAll() { r.dropAll() }

// RunRound 测试一轮并回报
func (r *Runner) RunRound(ctx context.Context) {
	r.mu.Lock()
	cfg := r.cfg
	list := make([]*entry, 0, len(r.order))
	for _, id := range r.order {
		list = append(list, r.entries[id])
	}
	r.mu.Unlock()
	if len(list) == 0 {
		return
	}
	tt := r.Tester
	if tt == nil {
		timeout := time.Duration(cfg.TimeoutMS) * time.Millisecond
		if timeout <= 0 {
			timeout = defaultTimeoutMS * time.Millisecond
		}
		tt = &tester.Tester{URLs: cfg.TestURLs, Timeout: timeout}
	}
	conc := cfg.Concurrency
	if conc <= 0 {
		conc = defaultConc
	}

	start := time.Now()
	results := make([]panel.Result, len(list))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, e := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, e *entry) {
			defer wg.Done()
			defer func() { <-sem }()
			if e.err != nil {
				results[i] = panel.Result{NodeID: e.target.NodeID, Status: tester.StatusProxyFail,
					Error: "invalid proxy config: " + e.err.Error(), CheckedAt: time.Now().Unix()}
				return
			}
			results[i] = tt.Test(ctx, e.target, e.proxy)
			if results[i].Status != tester.StatusOK {
				r.Log.Debug("node failed", "node", tester.Describe(e.target), "status", results[i].Status, "err", results[i].Error)
			}
		}(i, e)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	ok := 0
	for _, res := range results {
		if res.Status == tester.StatusOK {
			ok++
		}
	}
	r.Log.Info("round done", "ok", ok, "total", len(results), "took", time.Since(start).Round(time.Millisecond))
	r.report(ctx, panel.Report{Version: r.Version, RoundID: newRoundID(), Results: results})
}

// report 先补报缓存的轮次，再报本轮；失败的留到下次（超过 1 小时的丢弃）
func (r *Runner) report(ctx context.Context, rep panel.Report) {
	r.mu.Lock()
	queue := append(r.pending, rep)
	r.pending = nil
	r.mu.Unlock()

	cutoff := time.Now().Add(-pendingMaxAge).Unix()
	var keep []panel.Report
	for i, item := range queue {
		if len(item.Results) > 0 && item.Results[0].CheckedAt < cutoff {
			continue
		}
		if err := r.Panel.Report(ctx, item); err != nil {
			r.Log.Warn("report failed, will retry next round", "err", err, "pending", len(queue)-i)
			keep = append(keep, queue[i:]...)
			break
		}
	}
	r.mu.Lock()
	r.pending = append(keep, r.pending...)
	r.mu.Unlock()
}

// Pending 待补报的轮数（测试用）
func (r *Runner) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func newRoundID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return time.Now().UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
