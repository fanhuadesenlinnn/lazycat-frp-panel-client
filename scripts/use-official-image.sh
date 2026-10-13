#!/usr/bin/env bash
# 把 lzc-manifest.yml 的 application.image 换成懒猫官方源（registry.lazycat.cloud）引用。
#
#   scripts/use-official-image.sh registry.lazycat.cloud/<社区用户名>/lazycat-frp-panel-client:<tag>
#
# 用法：先在上架前把镜像转存到官方源——
#   lzc-cli appstore login
#   lzc-cli appstore copy-image ghcr.io/fanhuadesenlinnn/lazycat-frp-panel-client:<version>
# 它会打印形如 registry.lazycat.cloud/<社区用户名>/lazycat-frp-panel-client:<镜像ID> 的引用，
# 把那串引用作为本脚本的参数即可（lpk lint 的上架检查要求镜像来自官方源）。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

REF="${1:-}"
if [ -z "$REF" ]; then
  echo "用法: scripts/use-official-image.sh registry.lazycat.cloud/<用户名>/lazycat-frp-panel-client:<tag>" >&2
  exit 1
fi
case "$REF" in
  registry.lazycat.cloud/*) ;;
  *)
    echo "错误: 官方源引用必须以 registry.lazycat.cloud/ 开头（lpk lint 的上架检查会拒绝其它 registry）" >&2
    exit 1
    ;;
esac

# manifest 里只有一个以 image: 开头的键（application.image），改它即可。
count="$(grep -c '^[[:space:]]*image:' lzc-manifest.yml || true)"
if [ "$count" != "1" ]; then
  echo "错误: lzc-manifest.yml 里有 $count 处 image: 行，无法自动定位 application.image" >&2
  exit 1
fi

tmp="$(mktemp)"
sed "s#^\([[:space:]]*\)image: .*#\1image: ${REF}#" lzc-manifest.yml > "$tmp"

if cmp -s "$tmp" lzc-manifest.yml; then
  rm -f "$tmp"
  echo "application.image 已经是 $REF，无需修改。"
  exit 0
fi

mv "$tmp" lzc-manifest.yml
echo "已更新 lzc-manifest.yml: application.image = $REF"
echo "提醒："
echo "  1) 更新 package.yml 的包装版本，并重新打包、lint和设备验证；"
echo "  2) 新 v* 标签发布会复用官方镜像，不重复推送镜像；"
echo "  3) 商店资料与手动提交步骤见 docs/STORE-SUBMISSION.md。"
