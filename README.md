# NPprobe 测试端

部署在目标地区（首先是俄罗斯）的 VPS 上，用**真实代理协议**定期测试 NPanel 的节点，判断节点在该地区能不能用、延迟多少，结果回报面板，异常时由面板发 Telegram 告警。

设计方案见 NPanelPro 仓库 `NPanel-backend/doc/测试端设计.md`（Bear_branch）。

## 要点

- **内核**：把 [mihomo](https://github.com/MetaCubeX/mihomo)（≥ v1.19.34）当作 Go 库引入，覆盖节点的全部协议：vless（Reality / vision / xhttp / encryption）、vmess、trojan、ss2022、hysteria2（salamander、端口跳跃）、TUIC、anytls。
- **配置来源**：面板 `GET /v1/probe/targets` 下发每个节点的 Clash 代理配置（由线上 Clash 订阅模板渲染，与用户拿到的一致）和测试参数。
- **测试**：每轮（默认 5 分钟 ± 抖动）对每个节点做 TCP 连通 + 经代理请求 `generate_204`，失败重试一次。
- **回报**：`POST /v1/probe/report` 批量上报，同时作为心跳；失败时本地缓存补报。
- **身份**：面板为每个测试端注入一个虚拟用户（保留 ID 区间，不计费、不算在线、不限设备）。

## 部署

在面板 → 运维管理 → 测试端 里「添加测试端」，复制弹出的安装命令，在测试端 VPS 上用 root 执行：

```bash
bash <(curl -Ls https://raw.githubusercontent.com/yunsen1599/NPprobe/main/scripts/install.sh) --panel https://panel.example.com/api --token npp_xxx
```

- 安装到 `/usr/local/bin/npprobe`，配置 `/etc/npprobe/config.yml`（见 `config.example.yml`），systemd 服务 `npprobe.service`
- 日志：`journalctl -u npprobe -f`；卸载：同一个脚本加 `uninstall`
- 安装包从本仓库的 GitHub Release 下载并按 `checksums.txt` 校验；要换发布仓库时，发布设 `RELEASE_REPO`、安装设 `NPPROBE_REPO`

## 开发

```bash
go test ./...            # 含经 mihomo 真实代理链路的测试（本地 CONNECT 代理 + 204 服务）
go build -o npprobe .
./npprobe -c config.yml
scripts/release.sh 0.1.0 # 编译 linux amd64/arm64，创建 Release
```

代码结构：`internal/panel`（面板接口）、`internal/tester`（单个节点的测试步骤、测速）、`internal/runner`（拉目标、每轮测试、回报与补报）、`internal/upgrade`（自升级）。

## 测速与自升级

- **测速**：面板下发 `speed_test`（默认每个节点每 12 轮一次，经代理下载 2 MB），结果随本轮回报的 `speed_kbps` 上报。
- **自升级**：面板 → 测速管理 → 测试端列表 →「升级」设目标版本（`latest` 或 `x.y.z`）。测试端拉到目标后从 `release_repo` 的 Release 下载对应架构的安装包，按 `checksums.txt` 校验，替换 `/usr/local/bin/npprobe` 后退出，由 systemd 重启。`dev` 版本不自升级。
- **补报缓存**：没报上去的轮次写到 `state_dir/pending.json`（默认 `/var/lib/npprobe`），重启或升级后继续补报，最多保留 1 小时。

## 许可证

GPL-3.0（见 `LICENSE`）。本程序以库的方式引入 [mihomo](https://github.com/MetaCubeX/mihomo)（GPL-3.0），按其要求以 GPL-3.0 开源发布。
