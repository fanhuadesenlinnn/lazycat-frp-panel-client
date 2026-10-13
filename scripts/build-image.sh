#!/usr/bin/env bash
# 构建懒猫应用镜像（Dockerfile.lpk），可选推送到 registry。
#
#   scripts/build-image.sh                # 只构建（amdg64/arm64 双架构，不进任何 registry）
#   scripts/build-image.sh --push         # 构建并推送
#   scripts/build-image.sh --push --tag 0.2.0
#
# 镜像名与标签默认取自 lzc-manifest.yml 里 application.image 那一行，
# 保证 LPK 引用的就是这里推上去的镜像；--repo/--tag 可显式覆盖。
#
# 依赖：docker buildx（多架构构建）；content/bin/linux-<arch>/ 已由
# scripts/build-binaries.sh 产出。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

DOCKER="${DOCKER:-docker}"
DOCKERFILE="${DOCKERFILE:-Dockerfile.lpk}"
PLATFORMS="${PLATFORMS:-linux/amd64,linux/arm64}"
PUSH=0
LATEST=1
REPO=""
TAG=""

usage() {
  cat <<'EOF'
用法: scripts/build-image.sh [选项]
  --push                 构建后推送到 registry（不加则只构建校验）
  --no-latest            不更新 latest 标签（测试版本使用）
  --repo <name>          覆盖镜像仓库名（默认取 lzc-manifest.yml 的 application.image）
  --tag <tag>            覆盖标签（默认取 lzc-manifest.yml 的 application.image）
  --platforms <list>     目标平台（默认 linux/amd64,linux/arm64）
  -h, --help             显示帮助
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --push) PUSH=1; shift ;;
    --no-latest) LATEST=0; shift ;;
    --repo) REPO="$2"; shift 2 ;;
    --tag) TAG="$2"; shift 2 ;;
    --platforms) PLATFORMS="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数: $1" >&2; usage; exit 1 ;;
  esac
done

if ! command -v "$DOCKER" >/dev/null 2>&1; then
  echo "未找到 docker，请安装 Docker（需要 buildx 以支持多架构构建）" >&2
  exit 1
fi
if ! "$DOCKER" buildx version >/dev/null 2>&1; then
  echo "docker buildx 不可用（多架构构建需要 buildx）" >&2
  exit 1
fi

VERSION="$(sed -n 's/^version:[[:space:]]*//p' package.yml | head -n1 | tr -d '"'"'"' \r')"
if [ -z "$VERSION" ]; then
  echo "无法从 package.yml 读取 version" >&2
  exit 1
fi

IMAGE_REF="$(sed -n 's/^[[:space:]]*image:[[:space:]]*//p' lzc-manifest.yml | head -n1 | tr -d '"'"'"' \r')"
if [ -z "$IMAGE_REF" ]; then
  echo "无法从 lzc-manifest.yml 读取 application.image；LPK 会退回系统默认镜像，请先设置该项" >&2
  exit 1
fi

# 拆出仓库名与标签：只有最后一个 '/' 之后出现的 ':' 才算标签分隔符，
# 这样 registry:5000/xxx 这种带端口的仓库名不会被误切。
if [[ "$IMAGE_REF" == *:* && "${IMAGE_REF##*:}" != */* ]]; then
  MANIFEST_REPO="${IMAGE_REF%:*}"
  MANIFEST_TAG="${IMAGE_REF##*:}"
else
  MANIFEST_REPO="$IMAGE_REF"
  MANIFEST_TAG=""
fi
REPO="${REPO:-$MANIFEST_REPO}"
TAG="${TAG:-${MANIFEST_TAG:-$VERSION}}"

# 官方源引用的是已转存的不可变镜像；不能用本机构建覆盖该标签。
if [[ "$PUSH" = 1 && "$REPO" == registry.lazycat.cloud/* ]]; then
  echo "官方源镜像已转存，本脚本不向 registry.lazycat.cloud 推送。只需重新打包 LPK。" >&2
  exit 1
fi

for arch in ${PLATFORMS//,/ }; do
  arch="${arch##*/}"
  for bin in lzc-frpp-client frp-panel-client; do
    if [ ! -f "content/bin/linux-$arch/$bin" ]; then
      echo "错误: 缺少 content/bin/linux-$arch/$bin，请先运行 scripts/build-binaries.sh" >&2
      exit 1
    fi
  done
done

if [ "$TAG" != "$VERSION" ]; then
  echo "提示: 镜像标签 ($TAG) 与 package.yml 的版本 ($VERSION) 不同（转存官方源时标签会是镜像 ID，属正常情况）" >&2
fi

args=(buildx build --platform "$PLATFORMS" -f "$DOCKERFILE" -t "$REPO:$TAG")
if [ "$TAG" = "$VERSION" ] && [ "$LATEST" = 1 ]; then
  args+=(-t "$REPO:latest")
fi

mkdir -p dist
META="dist/.image-metadata.json"
rm -f "$META"

if [ "$PUSH" = "1" ]; then
  args+=(--push --metadata-file "$META")
  echo "==> docker ${args[*]} ."
  "$DOCKER" "${args[@]}" .
  DIGEST=""
  if [ -f "$META" ]; then
    DIGEST="$(node -e 'const fs=require("fs");const m=JSON.parse(fs.readFileSync(process.argv[1],"utf8"));process.stdout.write(m["containerimage.digest"]||"")' "$META" 2>/dev/null || true)"
  fi
  {
    echo "# 本次发布引用的应用镜像"
    echo "image: $REPO:$TAG"
    [ -n "$DIGEST" ] && echo "digest: $DIGEST"
    echo "platforms: $PLATFORMS"
  } > dist/IMAGE.txt
  echo "==> 已推送 $REPO:$TAG${DIGEST:+ (digest $DIGEST)}"
  echo "==> 已写入 dist/IMAGE.txt"
else
  # 不带输出的多平台构建只验证 Dockerfile 与 COPY 源是否可用，
  # 结果留在构建缓存里，不产生本地镜像。
  echo "==> docker ${args[*]} .（仅构建校验，不推送）"
  "$DOCKER" "${args[@]}" .
  echo "==> 构建成功（未推送）。加 --push 才会推到 registry。"
fi
