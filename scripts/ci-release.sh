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
# lzc-cli 2.0.9 没有 `project build` 子命令，正式打包用 `project release`。
lzc-cli project release -f lzc-build.yml -o "dist/lazycat-frp-panel-client-$version.lpk"
# 兜底：确保 content.tar 内的 start.sh 与 bin/** 带可执行位（tar mode 直接取自
# 文件系统，如果 CI 检出或打包环节丢了 +x，安装后 exec:// 路由会失败）。
node scripts/fix-lpk-modes.mjs "dist/lazycat-frp-panel-client-$version.lpk" --in-place
lzc-cli lpk lint "dist/lazycat-frp-panel-client-$version.lpk"
lzc-cli lpk info "dist/lazycat-frp-panel-client-$version.lpk"
(cd dist && sha256sum *.lpk *.tar.gz > SHA256SUMS)
