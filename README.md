# NPprobe 测试端

部署在目标地区（首先是俄罗斯）的 VPS 上，用**真实代理协议**定期测试 NPanel 的节点，判断节点在该地区能不能用、延迟多少，结果回报面板，异常时由面板发 Telegram 告警。

设计方案见 NPanelPro 仓库 `NPanel-backend/doc/测试端设计.md`（Bear_branch）。

## 要点

- **内核**：把 [mihomo](https://github.com/MetaCubeX/mihomo)（≥ v1.19.34）当作 Go 库引入，覆盖节点的全部协议：vless（Reality / vision / xhttp / encryption）、vmess、trojan、ss2022、hysteria2（salamander、端口跳跃）、TUIC、anytls。
- **配置来源**：面板 `GET /v1/probe/targets` 下发每个节点的 Clash 代理配置（由线上 Clash 订阅模板渲染，与用户拿到的一致）和测试参数。
- **测试**：每轮（默认 5 分钟 ± 抖动）对每个节点做 TCP 连通 + 经代理请求 `generate_204`，失败重试一次。
- **回报**：`POST /v1/probe/report` 批量上报，同时作为心跳；失败时本地缓存补报。
- **身份**：面板为每个测试端注入一个虚拟用户（保留 ID 区间，不计费、不算在线、不限设备）。

## 部署（规划）

```bash
bash <(curl -Ls <安装脚本地址>) --panel https://panel.example.com --token <token>
```

systemd 服务 `npprobe.service`，配置 `/etc/npprobe/config.yml`。

## 许可证

mihomo 是 GPL-3.0。本程序只在自有 VPS 上运行没有问题；如果把二进制分发给第三方，需要按 GPL-3.0 一并提供源码。
