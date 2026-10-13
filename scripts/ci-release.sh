#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

upstream="${UPSTREAM_DIR:-$PWD/.upstream/frp-panel}"
expected=1a58b856d7de19de8669b7072872986d2fa1604a
actual=$(git -C "$upstream" rev-parse HEAD)
if [[ "$actual" != "$expected" ]]; then
  echo "Upstream revision mismatch: $actual" >&2
  exit 1
fi
mkdir -p dist content/bin
version=$(node -e "const fs=require('fs'); const m=fs.readFileSync('package.yml','utf8').match(/^version:\\s*[\"']?([^\s\"']+)/m); if(!m)process.exit(1); process.stdout.write(m[1])")
for arch in amd64 arm64; do
  out="$PWD/content/bin/linux-$arch"
  mkdir -p "$out"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$out/lzc-frpp-client" .
  (
    cd "$upstream"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -mod=readonly -trimpath \
      -ldflags "-s -w -X github.com/VaalaCat/frp-panel/conf.gitCommit=$expected -X github.com/VaalaCat/frp-panel/conf.gitVersion=1a58b85" \
      -o "$out/frp-panel-client" ./cmd/frppc
  )
  chmod 755 "$out/lzc-frpp-client" "$out/frp-panel-client"
done
find content -type f -name '*.sh' -exec chmod 755 {} +
git -C "$upstream" archive --format=tar.gz --output="$PWD/dist/frp-panel-source-$expected.tar.gz" HEAD
git archive --format=tar.gz --output="$PWD/dist/lazycat-client-source.tar.gz" HEAD
# 应用镜像必须显式声明：LPK 是 image-only 包，二进制不在包里，全靠这条引用。
image_ref=$(sed -n 's/^[[:space:]]*image:[[:space:]]*//p' lzc-manifest.yml | head -n1 | tr -d '"'"'"' \r')
if [ -z "$image_ref" ]; then
  echo "错误: lzc-manifest.yml 未设置 application.image，装到设备上会没有管理器可执行文件。" >&2
  exit 1
fi
echo "==> 应用镜像: $image_ref"
lpk="dist/lazycat-frp-panel-client-$version.lpk"
# lzc-cli 2.0.9 没有 `project build` 子命令，正式打包用 `project release`。
lzc-cli project release -f lzc-build.yml -o "$lpk"
# image-only 包：LPK 内只有 manifest.yml / package.yml / icon.png。
# 一旦体积回到 100 MB 级别，说明 contentdir 被加回来了（二进制重新进了包），
# 这是本仓库刻意避免的形态，必须直接失败而不是发出一个"大包"。
lpk_size=$(wc -c < "$lpk" | tr -d '[:space:]')
max_lpk_size=$((5 * 1024 * 1024))
if [ "$lpk_size" -gt "$max_lpk_size" ]; then
  echo "错误: LPK 体积 ${lpk_size} 字节，超出 image-only 包的预期上限 ${max_lpk_size} 字节。" >&2
  echo "请检查 lzc-build.yml 是否误配了 contentdir。" >&2
  exit 1
fi
echo "==> LPK 体积: ${lpk_size} 字节（image-only 包）"
# `lpk lint` 检查的是"能否上架商店"。其中"镜像必须来自 registry.lazycat.cloud"
# 这一条，只有在用 lzc-cli appstore copy-image 把镜像转存到官方源之后才能满足；
# 那一步需要懒猫开发者账号，CI 里做不了。因此这里只放行这一条已知告警并显式打印，
# 其它任何 lint 结果仍然让构建失败。
lint_out=""
if lint_out="$(lzc-cli lpk lint "$lpk" 2>&1)"; then
  echo "$lint_out"
else
  echo "$lint_out"
  if echo "$lint_out" | grep -q "requires \`application.image\` to start with registry.lazycat.cloud"; then
    echo "警告: 未通过上架检查——当前镜像来自 $image_ref。" >&2
    echo "      可以安装使用；上架商店前请执行：" >&2
    echo "        lzc-cli appstore copy-image $image_ref" >&2
    echo "        scripts/use-official-image.sh registry.lazycat.cloud/<用户名>/lazycat-frp-panel-client:<tag>" >&2
  else
    echo "错误: LPK lint 未通过，且不是已知的上架镜像来源告警。" >&2
    exit 1
  fi
fi
lzc-cli lpk info "$lpk"
(cd dist && sha256sum *.lpk *.tar.gz > SHA256SUMS)
