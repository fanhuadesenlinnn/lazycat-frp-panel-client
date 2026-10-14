#!/usr/bin/env bash
# 用 lzc-cli 打包 image-only LPK；无需本地二进制或 Docker。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

LZC_CLI="${LZC_CLI:-lzc-cli}"
OUT="${1:-$ROOT/dist/frppcsm.lpk}"

if ! command -v "$LZC_CLI" >/dev/null 2>&1; then
  echo "未找到 lzc-cli，请先安装：npm i -g @lazycatcloud/lzc-cli" >&2
  exit 1
fi

mkdir -p "$(dirname "$OUT")"
echo "==> lzc-cli project release -f lzc-build.yml -o $OUT"
# 显式指定 lzc-build.yml：project 系列命令默认会优先读取 lzc-build.dev.yml，
# 发布产物必须使用正式构建配置。
"$LZC_CLI" project release -f lzc-build.yml -o "$OUT"
# 打包后立刻校验产物结构：lpk info 失败说明产物不可用，必须让整个脚本失败，
# 不能吞掉错误后仍然宣称打包成功。
"$LZC_CLI" lpk info "$OUT"
"$LZC_CLI" lpk lint "$OUT"
echo "完成：$OUT"
