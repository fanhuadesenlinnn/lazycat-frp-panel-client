#!/usr/bin/env bash
# 用 lzc-cli 打包 .lpk（LPK v2）。要求 content/bin 下的二进制已就绪。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LZC_CLI="${LZC_CLI:-lzc-cli}"
OUT="${1:-$ROOT/dist/frp-panel-client.lpk}"

if ! command -v "$LZC_CLI" >/dev/null 2>&1; then
  echo "未找到 lzc-cli，请先安装：npm i -g @lazycatcloud/lzc-cli" >&2
  exit 1
fi

for arch in amd64 arm64; do
  for bin in lzc-frpp-client frp-panel-client; do
    if [ ! -f "content/bin/linux-$arch/$bin" ]; then
      echo "错误: 缺少 content/bin/linux-$arch/$bin，请先运行 scripts/build-binaries.sh" >&2
      exit 1
    fi
  done
done

mkdir -p "$(dirname "$OUT")"
echo "==> lzc-cli project release -f lzc-build.yml -o $OUT"
# 显式指定 lzc-build.yml：project 系列命令默认会优先读取 lzc-build.dev.yml，
# 发布产物必须使用正式构建配置。
"$LZC_CLI" project release -f lzc-build.yml -o "$OUT"
# 打包后立刻校验产物结构：lpk info 失败说明产物不可用，必须让整个脚本失败，
# 不能吞掉错误后仍然宣称打包成功。
"$LZC_CLI" lpk info "$OUT"
echo "完成：$OUT"
