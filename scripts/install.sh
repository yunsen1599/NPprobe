#!/usr/bin/env bash
# NPprobe 测试端一键安装 / 卸载。
#
#   安装：bash <(curl -Ls <本脚本地址>) --panel https://panel.example.com/api --token npp_xxx [--version 0.1.0]
#   卸载：bash <(curl -Ls <本脚本地址>) uninstall
#
# 安装到 /usr/local/bin/npprobe，配置 /etc/npprobe/config.yml，systemd 服务 npprobe.service。
# 安装包从 GitHub Release 下载（仓库可用环境变量 NPPROBE_REPO 覆盖），并按 checksums.txt 校验。
set -euo pipefail

REPO="${NPPROBE_REPO:-yunsen1599/NPprobe}"
BIN=/usr/local/bin/npprobe
CONF_DIR=/etc/npprobe
UNIT=/etc/systemd/system/npprobe.service

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
die() { red "错误：$*" >&2; exit 1; }

[[ $EUID -eq 0 ]] || die "请用 root 运行"
command -v systemctl >/dev/null || die "需要 systemd"

uninstall() {
    systemctl disable --now npprobe 2>/dev/null || true
    rm -f "$UNIT" "$BIN"
    rm -rf "$CONF_DIR" /var/lib/npprobe
    systemctl daemon-reload
    green "NPprobe 已卸载"
}

panel="" token="" version="latest"
while [[ $# -gt 0 ]]; do
    case "$1" in
        uninstall) uninstall; exit 0 ;;
        --panel) panel="${2:-}"; shift 2 ;;
        --token) token="${2:-}"; shift 2 ;;
        --version) version="${2:-}"; shift 2 ;;
        *) die "未知参数 $1" ;;
    esac
done
[[ "$panel" =~ ^https?://[^[:space:]]+$ ]] || die "--panel 必须是 http(s) 地址，例如 https://panel.example.com/api"
[[ "$token" =~ ^npp_[0-9a-f]{48}$ ]] || die "--token 格式不对（面板 → 测试端 里新建或重置后显示）"
[[ "$version" == latest || "$version" =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version 只能是 latest 或 x.y.z"

case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "不支持的架构 $(uname -m)" ;;
esac

for cmd in curl tar sha256sum; do
    command -v "$cmd" >/dev/null || die "缺少命令 $cmd"
done

if [[ "$version" == latest ]]; then
    base="https://github.com/$REPO/releases/latest/download"
else
    base="https://github.com/$REPO/releases/download/${version#v}"
fi
pkg="npprobe-linux-$arch.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "==> 下载 $pkg ($version)"
curl -fsSL --retry 3 -o "$work/$pkg" "$base/$pkg" || die "下载失败：$base/$pkg"
curl -fsSL --retry 3 -o "$work/checksums.txt" "$base/checksums.txt" || die "下载 checksums.txt 失败"
(cd "$work" && grep " $pkg\$" checksums.txt | sha256sum -c - >/dev/null) || die "安装包校验失败"
tar -xzf "$work/$pkg" -C "$work"

echo "==> 安装到 $BIN"
systemctl stop npprobe 2>/dev/null || true
install -m 0755 "$work/npprobe" "$BIN"

echo "==> 写入配置 $CONF_DIR/config.yml"
mkdir -p "$CONF_DIR"
umask 077
cat >"$CONF_DIR/config.yml" <<EOF
# NPprobe 测试端配置（由安装脚本生成）
panel: $panel
token: $token
log_level: info
# 自升级从这个仓库的 Release 下载
release_repo: $REPO
state_dir: /var/lib/npprobe
EOF
chmod 600 "$CONF_DIR/config.yml"

echo "==> 配置 systemd 服务"
cat >"$UNIT" <<'EOF'
[Unit]
Description=NPprobe node availability probe
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/npprobe -c /etc/npprobe/config.yml
Restart=always
RestartSec=10
LimitNOFILE=65535
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
# 补报缓存（/var/lib/npprobe）；自升级要替换 /usr/local/bin/npprobe
StateDirectory=npprobe
ReadWritePaths=/usr/local/bin

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now npprobe >/dev/null
sleep 3
if systemctl is-active --quiet npprobe; then
    green "NPprobe $("$BIN" -v) 已启动。几分钟后在面板 → 测试端 里看到它在线。"
    echo "查看日志：journalctl -u npprobe -f"
else
    red "NPprobe 没有启动成功，最近的日志："
    journalctl -u npprobe -n 30 --no-pager || true
    exit 1
fi
