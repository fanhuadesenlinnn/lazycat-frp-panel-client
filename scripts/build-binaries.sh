#!/usr/bin/env bash
# 编译管理器与上游 frp-panel 客户端，产出懒猫 content 目录结构：
#
#   content/bin/linux-amd64/lzc-frpp-client
#   content/bin/linux-amd64/frp-panel-client
#   content/bin/linux-arm64/lzc-frpp-client
#   content/bin/linux-arm64/frp-panel-client
#
# 上游客户端来源二选一：
#   1) 本地源码（默认 ../frp-panel，可用 --upstream-dir 指定）——可复现构建；
#   2) GitHub Release 预编译产物（--upstream-release <tag>），需要 sha256 校验。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

GO_BIN="${GO:-go}"
UPSTREAM_DIR="${UPSTREAM_DIR:-$ROOT/../frp-panel}"
UPSTREAM_RELEASE=""
ARCHES="amd64 arm64"
SKIP_UPSTREAM=0

usage() {
  cat <<'EOF'
用法: scripts/build-binaries.sh [选项]
  --upstream-dir <dir>       上游 frp-panel 源码目录（默认 ../frp-panel）
  --upstream-release <tag>   改为从 GitHub Release 下载 frp-panel-client-<os>-<arch>
  --arches "amd64 arm64"     目标架构列表（默认 "amd64 arm64"）
  --skip-upstream            只编译管理器，不动上游客户端
  -h, --help                 显示帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --upstream-dir) UPSTREAM_DIR="$2"; shift 2 ;;
    --upstream-release) UPSTREAM_RELEASE="$2"; shift 2 ;;
    --arches) ARCHES="$2"; shift 2 ;;
    --skip-upstream) SKIP_UPSTREAM=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

# shellcheck disable=SC1091
. "$ROOT/scripts/upstream-pins.env"

if ! command -v "$GO_BIN" >/dev/null 2>&1; then
  echo "未找到 go 命令，请安装 Go 1.24+ 或设置 GO=/path/to/go" >&2
  exit 1
fi

VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LD_FLAGS="-s -w -X main.version=${VERSION}+${BUILD_DATE}"

build_manager() {
  local arch="$1"
  local out="$ROOT/content/bin/linux-$arch/lzc-frpp-client"
  mkdir -p "$(dirname "$out")"
  echo "==> 编译管理器 linux/$arch"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" GOFLAGS=-trimpath \
    "$GO_BIN" build -trimpath -ldflags "$LD_FLAGS" -o "$out" .
  chmod 0755 "$out"
}

build_upstream_from_source() {
  local arch="$1"
  local out="$ROOT/content/bin/linux-$arch/frp-panel-client"
  echo "==> 从本地源码编译上游客户端 linux/$arch ($UPSTREAM_DIR)"
  (
    cd "$UPSTREAM_DIR"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
      "$GO_BIN" build -o "$out" ./cmd/frppc
  )
  chmod 0755 "$out"
}

# 源码构建只有在固定 commit 上才叫“可复现构建”：先核对 HEAD，不匹配就拒绝。
verify_upstream_commit() {
  local actual
  actual="$(git -C "$UPSTREAM_DIR" rev-parse HEAD 2>/dev/null || true)"
  if [ -z "$actual" ]; then
    echo "警告: $UPSTREAM_DIR 不是 git 仓库，无法核对上游 commit（SKIP_UPSTREAM_COMMIT_CHECK=1 可跳过）" >&2
    [ "${SKIP_UPSTREAM_COMMIT_CHECK:-0}" = "1" ] || {
      echo "错误: 拒绝在无法核对 commit 的源码树上构建" >&2
      exit 1
    }
    return 0
  fi
  if [ "$actual" != "$UPSTREAM_COMMIT" ]; then
    echo "错误: 上游源码 commit 不匹配" >&2
    echo "  期望: $UPSTREAM_COMMIT ($UPSTREAM_TAG)" >&2
    echo "  实际: $actual" >&2
    echo "如需在其它版本上构建，请显式设置 SKIP_UPSTREAM_COMMIT_CHECK=1。" >&2
    [ "${SKIP_UPSTREAM_COMMIT_CHECK:-0}" = "1" ] || exit 1
  else
    echo "==> 已核对上游 commit: $actual ($UPSTREAM_TAG)"
  fi
}

download_upstream() {
  local arch="$1"
  local out="$ROOT/content/bin/linux-$arch/frp-panel-client"
  local url="$UPSTREAM_REPO/releases/download/$UPSTREAM_RELEASE/frp-panel-client-linux-$arch"
  echo "==> 下载上游客户端 linux/$arch: $url"
  curl -fL --retry 3 -o "$out" "$url"
  chmod 0755 "$out"
  local want=""
  case "$arch" in
    amd64) want="$UPSTREAM_LINUX_AMD64_SHA256" ;;
    arm64) want="$UPSTREAM_LINUX_ARM64_SHA256" ;;
  esac
  if [ -z "$want" ]; then
    echo "错误: scripts/upstream-pins.env 中未填写 linux/$arch 的 sha256，拒绝使用未经校验的产物" >&2
    exit 1
  fi
  local got
  got="$(sha256sum "$out" | awk '{print $1}')"
  if [ "$got" != "$want" ]; then
    echo "错误: sha256 校验失败（期望 $want，实际 $got）" >&2
    exit 1
  fi
}

for arch in $ARCHES; do
  build_manager "$arch"
  if [ "$SKIP_UPSTREAM" = "1" ]; then
    continue
  fi
  if [ -n "$UPSTREAM_RELEASE" ]; then
    download_upstream "$arch"
  else
    if [ ! -d "$UPSTREAM_DIR/cmd/frppc" ]; then
      echo "错误: 未找到上游源码 $UPSTREAM_DIR/cmd/frppc" >&2
      echo "请用 --upstream-dir 指定源码目录，或使用 --upstream-release <tag>" >&2
      exit 1
    fi
    verify_upstream_commit
    build_upstream_from_source "$arch"
  fi
done

echo
echo "构建完成，内容目录："
find "$ROOT/content/bin" -type f -maxdepth 3 -printf '%10s  %p\n' 2>/dev/null || ls -lR "$ROOT/content/bin"
