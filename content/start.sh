#!/bin/sh
# 懒猫微服 frp-panel 客户端管理器的启动入口。
# 由 lzc-manifest.yml 的 exec:// 路由在应用容器内执行（懒猫应用镜像里位于
# /app/start.sh，二进制在 /app/bin/linux-<arch>/）。
#
# 这里只做架构分派 + 路径解析，不做任何用户输入拼接：参数要么来自镜像/包内
# 固定路径，要么是应用平台传入的参数（"$@"）。
set -eu

case "$0" in
  */*) SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) ;;
  *) SELF_DIR=$(CDPATH= cd -- "$(dirname -- "$(command -v -- "$0")")" && pwd) ;;
esac

case "$(uname -m)" in
  x86_64|amd64)
    ARCH=amd64
    ;;
  aarch64|arm64)
    ARCH=arm64
    ;;
  *)
    echo "不支持的系统架构: $(uname -m)" >&2
    exit 1
    ;;
esac

# 镜像布局：/app/start.sh + /app/bin/linux-<arch>/…
# 历史打包布局（二进制内嵌在 LPK 里）：/lzcapp/pkg/content/{start.sh,bin/…}
# 两种布局都用"脚本自己所在目录"来定位二进制，因此同一份脚本通用。
BASE="${SELF_DIR}/bin/linux-${ARCH}"

if [ ! -x "${BASE}/lzc-frpp-client" ]; then
  echo "未找到管理器可执行文件: ${BASE}/lzc-frpp-client" >&2
  exit 1
fi

# 懒猫部署：浏览器一定经过 lzc-ingress 才到达本进程，且管理器只监听回环地址，
# 因此可以信任 ingress 注入的 X-HC-User-ID / X-HC-User-Role。
# 正常路径由 lzc-manifest.yml 的 environment 显式设置 LZCFRPP_TRUST_INGRESS=1；
# 这里再做一层兜底（检测到确实运行在懒猫应用容器内就开启），并允许显式覆盖为 0。
TRUST_INGRESS="${LZCFRPP_TRUST_INGRESS:-}"
if [ -z "${TRUST_INGRESS}" ]; then
  if [ -n "${LAZYCAT_APP_ID:-}" ]; then
    TRUST_INGRESS=1
  else
    TRUST_INGRESS=0
  fi
fi

exec "${BASE}/lzc-frpp-client" \
  --listen=127.0.0.1:18080 \
  --data-dir=/lzcapp/var/frp-panel-client \
  --client-bin="${BASE}/frp-panel-client" \
  --require-admin="${LZCFRPP_REQUIRE_ADMIN:-1}" \
  --trust-ingress="${TRUST_INGRESS}" \
  "$@"
