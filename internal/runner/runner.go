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
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/metacubex/mihomo/adapter"
	C "github.com/metacubex/mihomo/constant"
	"github.com/yunsen1599/NPprobe/internal/panel"
	"github.com/yunsen1599/NPprobe/internal/tester"
	"github.com/yunsen1599/NPprobe/internal/upgrade"
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

// 统一延迟（同 mihomo / Clash 的 unified-delay）：建好连接后再请求一次，只计第二次的耗时，
// 去掉 TCP / TLS / 协议握手，结果接近「测试端 → 节点 → 测试地址」一个来回，和客户端里显示的口径一致。
// 第二次请求失败时 mihomo 退回第一次（含握手）的耗时，不会因此判为失败。
func init() { adapter.UnifiedDelay.Store(true) }

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
	// Upgrader 自升级；为空时不升级
	Upgrader *upgrade.Upgrader
	// StateDir 补报缓存文件所在目录；为空时只缓存在内存
	StateDir string

	mu          sync.Mutex
	cfg         panel.Config
	entries     map[int64]*entry
	order       []int64
	lastTargets time.Time
	pending     []panel.Report
	round       int64
	// upgradeFailed 升级失败的版本和时间，1 小时内不重试同一版本
	upgradeFailed   string
	upgradeFailedAt time.Time
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
	r.loadPending()
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
	// 自升级只在拿到新的测试目标后检查（目标版本随测试目标下发）
	if err := r.maybeUpgrade(ctx); errors.Is(err, upgrade.ErrRestart) {
		return err
	}
	for {
		if time.Since(r.lastTargets) >= targetsInterval {
			if err := r.refreshTargets(ctx); err != nil {
				r.Log.Warn("refresh targets failed, keep previous targets", "err", err)
				if errors.Is(err, panel.ErrUnauthorized) {
					r.dropAll()
				}
			} else if err := r.maybeUpgrade(ctx); errors.Is(err, upgrade.ErrRestart) {
				return err
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
		// 没有测试目标（面板里禁用了，或还没分配节点）也要报心跳，否则面板一直显示离线
		r.report(ctx, panel.Report{Version: r.Version, Results: []panel.Result{}})
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
	r.mu.Lock()
	round := r.round
	r.round++
	r.mu.Unlock()
	speed := cfg.SpeedTest

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
			if results[i].Status == tester.StatusOK && speed.URL != "" && tester.SpeedDue(e.target.NodeID, round, speed.EveryRounds) {
				results[i].SpeedKbps = r.speedTest(ctx, e, speed)
			}
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
			// 只有心跳、没有结果的不必补报
			for _, rest := range queue[i:] {
				if len(rest.Results) > 0 {
					keep = append(keep, rest)
				}
			}
			break
		}
	}
	r.mu.Lock()
	r.pending = append(keep, r.pending...)
	r.mu.Unlock()
	r.savePending()
}

// speedTest 经代理下载一次；失败或数据太少时返回 0（不影响本轮的连通结果）
func (r *Runner) speedTest(ctx context.Context, e *entry, cfg panel.SpeedTest) int64 {
	d, ok := e.proxy.(tester.Downloader)
	if !ok {
		return 0
	}
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = 2_000_000
	}
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	n, took, err := d.Download(sctx, cfg.URL, maxBytes)
	if err != nil {
		r.Log.Debug("speed test failed", "node", tester.Describe(e.target), "err", err)
		return 0
	}
	kbps := tester.Kbps(n, took)
	r.Log.Debug("speed test", "node", tester.Describe(e.target), "bytes", n, "took", took, "kbps", kbps)
	return kbps
}

// maybeUpgrade 面板要求的版本与当前不同时自升级；成功返回 upgrade.ErrRestart
func (r *Runner) maybeUpgrade(ctx context.Context) error {
	r.mu.Lock()
	target := r.cfg.TargetVersion
	failed, failedAt := r.upgradeFailed, r.upgradeFailedAt
	r.mu.Unlock()
	if r.Upgrader == nil || target == "" {
		return nil
	}
	uctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	version, err := r.Upgrader.Resolve(uctx, target)
	if err != nil {
		r.Log.Warn("resolve upgrade version failed", "target", target, "err", err)
		return err
	}
	if !r.Upgrader.Needed(version) || (version == failed && time.Since(failedAt) < time.Hour) {
		return nil
	}
	r.Log.Info("upgrading", "from", r.Version, "to", version)
	err = r.Upgrader.Apply(uctx, version)
	if errors.Is(err, upgrade.ErrRestart) {
		r.Log.Info("upgrade installed, restarting", "version", version)
		r.savePending()
		return err
	}
	r.mu.Lock()
	r.upgradeFailed, r.upgradeFailedAt = version, time.Now()
	r.mu.Unlock()
	r.Log.Error("upgrade failed, retry in 1h", "version", version, "err", err)
	return err
}

func (r *Runner) pendingFile() string {
	if r.StateDir == "" {
		return ""
	}
	return filepath.Join(r.StateDir, "pending.json")
}

// savePending 待补报的轮次写盘，进程重启（含自升级）后还能补报
func (r *Runner) savePending() {
	path := r.pendingFile()
	if path == "" {
		return
	}
	r.mu.Lock()
	raw, err := json.Marshal(r.pending)
	empty := len(r.pending) == 0
	r.mu.Unlock()
	if err != nil {
		return
	}
	if empty {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(r.StateDir, 0o700); err != nil {
		r.Log.Warn("save pending reports failed", "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err == nil {
		err = os.Rename(tmp, path)
		if err != nil {
			r.Log.Warn("save pending reports failed", "err", err)
		}
	}
}

func (r *Runner) loadPending() {
	path := r.pendingFile()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var pending []panel.Report
	if json.Unmarshal(raw, &pending) == nil {
		r.mu.Lock()
		r.pending = append(pending, r.pending...)
		r.mu.Unlock()
		r.Log.Info("loaded pending reports", "rounds", len(pending))
	}
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
