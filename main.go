// NPprobe 测试端：部署在目标地区的 VPS 上，用真实代理协议（mihomo 内核）定期测试 NPanel 的节点，
// 结果回报面板。设计见 NPanelPro 仓库 NPanel-backend/doc/测试端设计.md。
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/yunsen1599/NPprobe/internal/config"
	"github.com/yunsen1599/NPprobe/internal/panel"
	"github.com/yunsen1599/NPprobe/internal/runner"
)

// version 发布时由 -ldflags "-X main.version=x.y.z" 注入
var version = "dev"

func main() {
	cfgPath := flag.String("c", "/etc/npprobe/config.yml", "config file")
	showVersion := flag.Bool("v", false, "print version")
	flag.Parse()
	if *showVersion || (flag.NArg() > 0 && flag.Arg(0) == "version") {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load config:", err)
		os.Exit(1)
	}
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Info("NPprobe starting", "version", version, "panel", cfg.Panel)
	r := &runner.Runner{Panel: panel.New(cfg.Panel, cfg.Token, version), Version: version, Log: logger}
	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("exit", "err", err)
		os.Exit(1)
	}
	logger.Info("NPprobe stopped")
}
