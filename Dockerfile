# 非懒猫环境的可选部署方式（Docker / docker compose）。
#
# 构建前请先执行 scripts/build-binaries.sh，让 content/bin/linux-<arch>/ 下
# 同时具备 lzc-frpp-client 与 frp-panel-client 两个二进制。
#
#   scripts/build-binaries.sh --upstream-dir ../frp-panel
#   docker build -t lzc-frpp-client:0.1.0 ./lazycat-client
#   docker run -d --name frpp -p 18080:18080 -v frpp-data:/data \
#     -e LZCFRPP_ADMIN_TOKEN=$(openssl rand -hex 24) lzc-frpp-client:0.1.0
#
# 注意：容器内必须监听 0.0.0.0 才能被端口映射访问，因此这里显式使用了
# --allow-non-loopback-listen。请务必配合访问令牌或反向代理的鉴权使用，
# 不要把该端口直接暴露到公网。
#
# 独立部署不会信任 X-HC-User-ID / X-HC-User-Role 请求头（管理器在
# --allow-non-loopback-listen 下也会拒绝 -trust-ingress），因此必须使用
# 访问令牌登录：打开页面后输入 config.json 中的 admin_token（或用
# LZCFRPP_ADMIN_TOKEN 预设），登录成功后使用 HttpOnly 会话 Cookie。

FROM alpine:3.21

ARG TARGETARCH=amd64

RUN apk add --no-cache ca-certificates \
    && adduser -D -H -u 10001 frpp \
    && mkdir -p /data \
    && chown -R frpp:frpp /data

COPY content/bin/linux-${TARGETARCH}/lzc-frpp-client /app/lzc-frpp-client
COPY content/bin/linux-${TARGETARCH}/frp-panel-client /app/frp-panel-client
COPY content/licenses /app/licenses

RUN chmod 0755 /app/lzc-frpp-client /app/frp-panel-client

USER frpp
WORKDIR /data
VOLUME ["/data"]
EXPOSE 18080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:18080/healthz || exit 1

ENTRYPOINT ["/app/lzc-frpp-client"]
CMD ["--listen=0.0.0.0:18080", "--data-dir=/data", \
     "--client-bin=/app/frp-panel-client", "--allow-non-loopback-listen"]
