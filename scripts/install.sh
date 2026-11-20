#!/usr/bin/env bash
# NPprobe 测试端一键安装 / 卸载。
#
#   安装：bash <(curl -Ls <本脚本地址>) --panel https://panel.example.com/api --token npp_xxx [--version 0.1.0]
#   离线：bash install.sh --panel ... --token ... --package ./npprobe-linux-amd64.tar.gz [--checksums ./checksums.txt]
#   卸载：bash <(curl -Ls <本脚本地址>) uninstall
#
# 安装到 /usr/local/bin/npprobe，配置 /etc/npprobe/config.yml，systemd 服务 npprobe.service。
# 安装包从 GitHub Release 下载（仓库可用环境变量 NPPROBE_REPO 覆盖），并按 checksums.txt 校验；
# 缺少 curl / tar / sha256sum 时先用系统包管理器补装，curl 装不上时改用 wget。
# 退出码 20 表示下载不了安装包（没有下载工具或连不上 GitHub）：面板远程安装据此改为直传安装包。
set -euo pipefail

REPO="${NPPROBE_REPO:-yunsen1599/NPprobe}"
BIN=/usr/local/bin/npprobe
CONF_DIR=/etc/npprobe
UNIT=/etc/systemd/system/npprobe.service

red() { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
die() { red "错误：$*" >&2; exit 1; }
die_download() { red "错误：$*" >&2; exit 20; }

[[ $EUID -eq 0 ]] || die "请用 root 运行"
command -v systemctl >/dev/null || die "需要 systemd"

uninstall() {
    systemctl disable --now npprobe 2>/dev/null || true
    rm -f "$UNIT" "$BIN"
    rm -rf "$CONF_DIR" /var/lib/npprobe
    systemctl daemon-reload
    green "NPprobe 已卸载"
}

panel="" token="" version="latest" package="" checksums=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        uninstall) uninstall; exit 0 ;;
        --panel) panel="${2:-}"; shift 2 ;;
        --token) token="${2:-}"; shift 2 ;;
        --version) version="${2:-}"; shift 2 ;;
        --package) package="${2:-}"; shift 2 ;;
        --checksums) checksums="${2:-}"; shift 2 ;;
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

# pkg_install 用系统包管理器安装软件包（尽力而为，失败返回非 0）
pkg_install() {
    local t=""
    command -v timeout >/dev/null && t="timeout 300"
    echo "==> 安装依赖：$*"
    if command -v apt-get >/dev/null; then
        DEBIAN_FRONTEND=noninteractive $t apt-get update -qq >/dev/null 2>&1 || true
        DEBIAN_FRONTEND=noninteractive $t apt-get install -y -qq "$@" >/dev/null 2>&1
    elif command -v dnf >/dev/null; then
        $t dnf install -y -q "$@" >/dev/null 2>&1
    elif command -v yum >/dev/null; then
        $t yum install -y -q "$@" >/dev/null 2>&1
    elif command -v zypper >/dev/null; then
        $t zypper --non-interactive --quiet install "$@" >/dev/null 2>&1
    elif command -v pacman >/dev/null; then
        $t pacman -Sy --noconfirm --needed "$@" >/dev/null 2>&1
    else
        return 1
    fi
}

# need 确保命令存在，缺少时安装对应的软件包
need() {
    local cmd="$1" pkg="$2"
    command -v "$cmd" >/dev/null && return 0
    pkg_install "$pkg" || true
    command -v "$cmd" >/dev/null
}

need tar tar || die "缺少命令 tar，自动安装失败，请先手动安装 tar"
need sha256sum coreutils || die "缺少命令 sha256sum，自动安装失败，请先手动安装 coreutils"

# fetch URL 文件：优先 curl，没有就用 wget
fetch() {
    if command -v curl >/dev/null; then
        curl -fsSL --retry 3 --connect-timeout 15 -o "$2" "$1"
    else
        wget -q --tries=3 --timeout=30 -O "$2" "$1"
    fi
}

pkg="npprobe-linux-$arch.tar.gz"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

if [[ -n "$package" ]]; then
    # 离线 / 面板直传：使用本地安装包
    [[ -f "$package" ]] || die "安装包不存在：$package"
    echo "==> 使用本地安装包 $package"
    cp "$package" "$work/$pkg"
    if [[ -n "$checksums" ]]; then
        [[ -f "$checksums" ]] || die "校验文件不存在：$checksums"
        cp "$checksums" "$work/checksums.txt"
        (cd "$work" && grep " $pkg\$" checksums.txt | sha256sum -c - >/dev/null) || die "安装包校验失败（架构是 $arch，安装包需要是 $pkg）"
    else
        echo "    未提供 checksums.txt，跳过校验"
    fi
else
    if ! command -v curl >/dev/null && ! command -v wget >/dev/null; then
        pkg_install curl || true
        command -v curl >/dev/null || command -v wget >/dev/null ||
            die_download "没有 curl 也没有 wget，自动安装 curl 失败（软件源不可用？）"
    fi
    if [[ "$version" == latest ]]; then
        base="https://github.com/$REPO/releases/latest/download"
    else
        base="https://github.com/$REPO/releases/download/${version#v}"
    fi
    echo "==> 下载 $pkg ($version)"
    fetch "$base/$pkg" "$work/$pkg" || die_download "下载失败：$base/$pkg（这台机器可能连不上 GitHub）"
    fetch "$base/checksums.txt" "$work/checksums.txt" || die_download "下载 checksums.txt 失败"
    (cd "$work" && grep " $pkg\$" checksums.txt | sha256sum -c - >/dev/null) || die "安装包校验失败"
fi
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
