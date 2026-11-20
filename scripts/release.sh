#!/usr/bin/env bash
# 发布 NPprobe：编译 linux amd64 / arm64，打包成 npprobe-linux-<arch>.tar.gz，
# 在发布仓库创建 Release <版本号> 并上传安装包和 checksums.txt，然后在源码仓库打同名 tag。
#
# 用法：scripts/release.sh <版本号>      例如 scripts/release.sh 0.1.0
#
# 发布仓库默认是本仓库；如果本仓库保持私有，用 RELEASE_REPO 指向一个公开仓库
# （install.sh 也要用 NPPROBE_REPO 指向同一个仓库），参考 NPnode → Bearnode 的做法。
# 依赖：go、tar、gh（已登录且对发布仓库有写权限）、git、shasum
set -euo pipefail

RELEASE_REPO="${RELEASE_REPO:-yunsen1599/NPprobe}"

version="${1:-}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "用法: $0 <x.y.z>" >&2; exit 1; }

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
[[ -z "$(git status --porcelain)" ]] || { echo "有未提交的修改，请先提交再发布" >&2; exit 1; }
if gh release view "$version" --repo "$RELEASE_REPO" >/dev/null 2>&1; then
    echo "发布仓库已存在 Release $version" >&2
    exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/dist"
for arch in amd64 arm64; do
    echo "==> 编译 linux/$arch"
    pkg="$work/pkg-$arch"
    mkdir -p "$pkg"
    GOOS=linux GOARCH="$arch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w -buildid= -X main.version=$version" -o "$pkg/npprobe" .
    tar -C "$pkg" -czf "$work/dist/npprobe-linux-$arch.tar.gz" npprobe
done
(cd "$work/dist" && shasum -a 256 *.tar.gz > checksums.txt)

echo "==> 创建 Release $version（$RELEASE_REPO）"
gh release create "$version" --repo "$RELEASE_REPO" --title "$version" --notes "NPprobe $version" \
    "$work"/dist/*.tar.gz "$work/dist/checksums.txt" scripts/install.sh

if [[ "$RELEASE_REPO" == "$(gh repo view --json nameWithOwner -q .nameWithOwner)" ]]; then
    echo "==> 发布仓库就是源码仓库，Release 已带 tag"
else
    git tag "$version"
    git push origin "$version"
fi
echo "完成"
