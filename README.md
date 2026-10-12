# 懒猫 frp-panel 客户端（lzc-frpp-client）

在懒猫微服（LazyCat）上以**应用**的形式运行上游 [frp-panel](https://github.com/VaalaCat/frp-panel)
的 **客户端**（`frp-panel client`），并提供中文管理界面来管理这个客户端进程与它的连接参数。

典型用途：把家里的内网设备 / NAS / 局域网服务注册到你**自建的 frp-panel Master** 上，
由 Master 统一编排暴露；本应用负责在该设备上稳定地跑客户端，并在一个页面里完成
配置、启动、停止、重启、查看日志。

> 本应用**不含** WireGuard 组网与 Serverless Worker（Cloudflare Worker）的管理界面，
> 也不修改上游任何代码：它把上游 `frp-panel client` 当作独立子进程来管理与展示。

---

## 目录结构

```
lazycat-client/
├── main.go                     # 管理器入口（纯标准库）
├── httpserver.go               # HTTP 服务器超时等参数
├── internal/
│   ├── config/                 # 持久化配置（config.json，0600，原子写入）
│   ├── redact/                 # 密钥脱敏（子进程输出先脱敏再入缓冲）
│   ├── logbuf/                 # 有界环形日志缓冲（先脱敏、再截断）
│   ├── upstream/               # 解析上游启动命令 + 生成 argv/env（不经过 shell）
│   ├── supervisor/             # 子进程生命周期与退避重启
│   └── web/                    # HTTP API + 内嵌中文前端（assets/）
├── content/
│   ├── start.sh                # 懒猫 exec:// 路由的启动入口（架构分派）
│   ├── bin/linux-<arch>/       # 打包进 LPK 的两个二进制（构建产物，不入库）
│   └── licenses/NOTICE.txt     # 上游来源与许可证说明
├── lzc-manifest.yml            # 运行结构（route/background_task/环境变量）
├── lzc-build.yml               # 正式打包配置
├── lzc-build.dev.yml           # 开发态差异（仅覆盖包名）
├── package.yml                 # LPK v2 静态元数据
├── Dockerfile                  # 非懒猫环境（Docker）可选部署
└── scripts/                    # 构建 / 打包 / 图标生成脚本
```

管理器不依赖任何第三方 Go 模块（`go.mod` 无 `require`），便于审计与离线构建。

---

## 快速开始（懒猫微服）

1. 安装本应用（应用商店或本地 LPK 安装包，见下文「打包与安装」）。
2. 在 **frp-panel 主控（Master）的 WebUI** 里，进入**客户端**页面，**创建一个客户端节点**
   （名称随意，例如 `home-nas`）。
3. 创建完成后，复制页面上给出的启动命令，形如：

   ```
   ./frp-panel client -s <客户端密钥> -i user.c.xxxx --api-url http://your-master:9000 --rpc-url grpc://your-master:9001
   ```

4. 打开本应用 → 把这条命令**整条粘贴**到「粘贴上游启动命令」输入框 → 点击
   **解析、保存并启动**。
5. 页面顶部状态变为「运行中」，下方日志出现客户端输出即表示管理器已成功拉起客户端。
   此时回到 Master 的客户端页面，该节点的连接状态由 Master 判断（本应用无法代它确认）。

也可以不使用粘贴命令：直接在「连接配置」里手工填写**客户端 ID / 客户端密钥 /
API URL / RPC URL**，保存后点击「启动」。

### 网络模型（很重要）

- frp-panel 客户端运行在**本应用自己的容器网络命名空间**里，`127.0.0.1` 指的是
  **这个容器**，**不是**你的懒猫主机，也不是同局域网的其它设备。
- 因此，如果你要暴露的东西跑在懒猫宿主机上，请在配置文件里写**宿主机的局域网 IP**
  （例如 `192.168.1.20:8080`），而不是 `127.0.0.1:8080`；暴露同局域网内其它设备同理。
- 客户端**主动外连** Master（API/RPC），不需要任何入站端口；管理界面本身只监听
  **回环地址** `127.0.0.1:18080`，由懒猫 ingress 反向代理，不会暴露到宿主网络。
- 正因为管理端口不需要对外，这里**没有**使用 `network_mode: host`。

---

## 安全模型

管理器按「只可能通过懒猫入口访问」来设计，主要措施：

| 措施 | 说明 |
| --- | --- |
| 监听地址 | 默认只监听回环地址。监听非回环地址必须显式加 `-allow-non-loopback-listen`，并且会**拒绝**与 `-trust-ingress` 同时使用（否则任何人都能伪造身份头冒充管理员）。 |
| 懒猫身份 | 开启 `-trust-ingress` 时信任 ingress 注入的 `X-HC-User-ID` / `X-HC-User-Role`；默认 `-require-admin` 只允许管理员账号使用管理界面。懒猫部署由 `lzc-manifest.yml` 的 `LZCFRPP_TRUST_INGRESS=1` 显式开启（`content/start.sh` 另有一层兜底），且该开关只与回环监听共存。 |
| 访问令牌 | 独立部署（Docker / 本机直跑）默认**不信任**任何身份请求头，必须用访问令牌登录。首次启动会在数据目录生成 `admin_token`，也可用 `LZCFRPP_ADMIN_TOKEN` 预设。 |
| 会话 | `POST /api/login` 用令牌换取 **HttpOnly + SameSite=Strict** 会话 Cookie；令牌只出现在同源 JSON 请求体里，不写入地址栏（`?token=` 仅在一次性自动登录后立即用 `history.replaceState` 清除）。 |
| CSRF | 所有变更类请求必须带同源 `Origin`/`Referer`、自定义请求头 `X-Requested-With: lzc-frpp-client`，且请求体必须是 `application/json`；只读接口同样要求已认证。 |
| 密钥处理 | 客户端密钥只存放在容器内数据目录（`config.json`，权限 0600，临时文件 + rename 原子写入），**从不回传浏览器**（只返回 `secret_set` / `secret_length`）；传给子进程时走**环境变量**（`CLIENT_SECRET`），不进 `ps` 可见的 argv。 |
| 日志脱敏 | 子进程的每一行输出都先经过脱敏（已知密钥字面量 + `-s`/`--secret`/`token=`/`Bearer` 等模式）再进入有界环形缓冲，浏览器读到的永远是脱敏后的内容。 |
| 高危开关 | 上游的 `CLIENT_FEATURES_ENABLE_REMOTE_SHELL`（远程命令执行）与 `CLIENT_FEATURES_ENABLE_FUNCTIONS` **一律显式下发**，默认 `false`；需要在界面上显式勾选才会开启。 |
| 不执行 shell | 粘贴的命令在本进程内做 token 化解析（含管道、重定向、命令替换的文本直接拒绝），**不会**交给 shell；真正执行的 argv 由校验过的结构化字段重建。 |
| 不夸大状态 | 界面区分「本地进程状态」与「远端是否在线」。`/api/status` 里的 `remote.claim` 固定为 `unknown`，探针只表示本容器能否连上 Master 地址，**不能**作为「客户端已在线」的凭据。 |

---

## 配置项

界面字段 ↔ `config.json` 字段 ↔ 环境变量（传递给上游子进程）：

| 界面 | config.json | 子进程环境变量 | 说明 |
| --- | --- | --- | --- |
| 客户端 ID | `client_id` | `CLIENT_ID` | 形如 `user.c.client1`，只允许字母/数字/`.`/`_`/`-` |
| 客户端密钥 | `client_secret` | `CLIENT_SECRET` | 不包含空白或控制字符；留空保存表示不修改 |
| API URL | `api_url` | `CLIENT_API_URL` | 仅 `http://` / `https://`，不允许内含用户名密码或查询参数 |
| RPC URL | `rpc_url` | `CLIENT_RPC_URL` | 仅 `grpc://` / `ws://` / `wss://`（TLS 反代建议 `wss://`） |
| 日志级别 | `log_level` | `LOGGER_DEFAULT_LOGGER_LEVEL` / `LOGGER_FRP_LOGGER_LEVEL` | `debug` / `info` / `warn` / `error` |
| 自动启动 | `auto_start` | — | 管理器启动时自动拉起客户端 |
| 允许函数 | `enable_functions` | `CLIENT_FEATURES_ENABLE_FUNCTIONS` | 默认关闭 |
| 允许远程命令 | `enable_remote_shell` | `CLIENT_FEATURES_ENABLE_REMOTE_SHELL` | 默认关闭，高风险 |

管理器自身的命令行参数 / 环境变量：

| 参数 | 环境变量 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `LZCFRPP_LISTEN` | `127.0.0.1:18080` | 管理界面监听地址 |
| `-data-dir` | `LZCFRPP_DATA_DIR` | `/lzcapp/var/frp-panel-client`（容器内） | 持久化目录 |
| `-client-bin` | `LZCFRPP_CLIENT_BIN` | 自动查找 | 上游 `frp-panel-client` 路径 |
| `-trust-ingress` | `LZCFRPP_TRUST_INGRESS` | `false` | 信任懒猫 `X-HC-User-ID` / `X-HC-User-Role` |
| `-require-admin` | `LZCFRPP_REQUIRE_ADMIN` | `true` | 只允许管理员账号访问 |
| `-extra-origins` | `LZCFRPP_EXTRA_ORIGINS` | 空 | 额外允许的请求来源（逗号分隔） |
| `-allow-non-loopback-listen` | — | `false` | 允许监听非回环地址（不推荐） |
| `-insecure-no-auth` | — | `false` | 关闭鉴权（**仅限**回环地址本机调试） |
| `-probe-interval` | — | `30s` | Master 可达性探测间隔 |
| `-log-capacity` | — | `800` | 内存中保留的日志行数 |

---

## Docker / 独立部署（可选）

非懒猫环境可以用 Docker 运行同一个管理器。先构建二进制：

```bash
scripts/build-binaries.sh --upstream-dir ../frp-panel   # 需要固定 commit 的上游源码
docker build -t lzc-frpp-client:0.1.0 ./lazycat-client
docker run -d --name frpp -p 18080:18080 -v frpp-data:/data \
  -e LZCFRPP_ADMIN_TOKEN=$(openssl rand -hex 24) lzc-frpp-client:0.1.0
```

然后浏览器打开 `http://<主机>:18080`，输入上面这个令牌登录。

> 容器内必须监听 `0.0.0.0` 才能被端口映射访问，所以 Dockerfile 里显式带了
> `--allow-non-loopback-listen`；此时**不会**开启 `-trust-ingress`（否则任何人都能伪造
> `X-HC-User-ID` 冒充管理员）。请务必配合令牌或前置反向代理的鉴权使用，
> **不要**把该端口直接暴露到公网。

---

## 可复现构建

上游被固定在 commit `1a58b856d7de19de8669b7072872986d2fa1604a`
（该 commit 同时带有 `v0.1.37` 标签，可用 `git -C <upstream> tag --points-at 1a58b856d7de19de8669b7072872986d2fa1604a` 核对），
见 `scripts/upstream-pins.env`。构建脚本会在编译上游客户端之前**核对 `HEAD` 是否等于该 commit**，
不匹配就拒绝构建（确需放宽时可显式设置 `SKIP_UPSTREAM_COMMIT_CHECK=1`）。

```bash
# Linux / macOS / Git Bash
scripts/build-binaries.sh --upstream-dir ../frp-panel

# Windows PowerShell
pwsh -File scripts/build-binaries.ps1 -UpstreamDir ..\frp-panel
```

产物固定在：

```
content/bin/linux-amd64/{lzc-frpp-client, frp-panel-client}
content/bin/linux-arm64/{lzc-frpp-client, frp-panel-client}
```

上下游都不修改任何源码；管理器只是把上游客户端当子进程拉起。

---

## 打包与安装

需要 [lzc-cli](https://developer.lazycat.cloud/) ≥ 2.0.0（本项目用 2.0.9 验证）：

```bash
npm i -g @lazycatcloud/lzc-cli@2.0.9
lzc-cli project lint                 # 校验 manifest
scripts/build-lpk.sh                 # 等价于 lzc-cli project release -f lzc-build.yml -o dist/frp-panel-client.lpk
lzc-cli lpk info dist/frp-panel-client.lpk
```

注意 `lzc-cli project` 子命令默认会优先读取 `lzc-build.dev.yml`，因此打包脚本始终显式
`-f lzc-build.yml`，避免把开发态包名打进正式产物。

> **Windows 上打包的坑**：`lzc-cli` 用 node-tar 打包 `content/`，tar 头里的 `mode` 直接
> 取自文件系统，而 Windows 无法表达 `+x`，于是 `content/start.sh` 与 `content/bin/**`
> 会被打成 `0644`，装到设备后 `exec://` 路由会因为脚本不可执行而失败。
> 如果只能在 Windows 上打包，请补跑一次：
>
> ```bash
> node scripts/fix-lpk-modes.mjs dist/frp-panel-client.lpk --in-place
> ```
>
> 它只改写 tar 头里的 `mode` 字段并重算校验和（不动任何文件内容）。**正式产物请始终用
> Linux / GitHub Actions 构建**，因为那里文件系统本身就有正确的可执行位。

安装到设备：

```bash
lzc-cli lpk install dist/frp-panel-client.lpk
```

安装后打开应用（`subdomain: frpp`，实际域名取决于你的盒子配置），按「快速开始」粘贴
Master 给出的客户端启动命令即可。应用声明了 `application.background_task: true`，
页面关掉后管理器与客户端仍会继续运行。

### 用 GitHub Actions 构建的安装包

本仓库的 `.github/workflows/build.yml` 在 push `main` / PR / 手动触发时运行：
Linux 下先 `go vet` + `go test -race`，再检出固定 commit 的上游源码，交叉编译两个架构的
两个二进制，用 `lzc-cli` 打包成 `.lpk`，并把 `.lpk`、上下游源码归档与 `SHA256SUMS`
一起上传为构建产物（artifact，保留 30 天）。

下载方式：

```bash
gh run list -R fanhuadesenlinnn/lazycat-frp-panel-client
gh run download <run-id> -R fanhuadesenlinnn/lazycat-frp-panel-client -D dist/github-build
```

或者在 GitHub 仓库的 **Actions → 对应 workflow run → Artifacts** 里点选下载。

### 大版本发布（GitHub 流水线）

正式版本一律走 `.github/workflows/release.yml`：**推送 `v*` 标签即自动构建并发布
GitHub Release**，不需要在本地打包。

```bash
# 1) 先把 package.yml 的 version 改好（必须与标签一致，例如 0.1.0 -> 0.2.0）并提交
git commit -am "release: v0.2.0"
# 2) 打带说明的标签，连同提交一起推送
git tag -a v0.2.0 -m "v0.2.0"
git push origin main --follow-tags
```

流水线依次执行：

1. 检出该标签对应的代码，跑 `go vet ./...` 与 `go test -race -count=1 ./...`；
2. 检出固定 commit 的上游源码，交叉编译 amd64 / arm64 两个架构的两个二进制；
3. `lzc-cli project release` 打包 LPK，兜底修正 `content.tar` 内的可执行位，再跑 `lpk lint`；
4. 校验 `SHA256SUMS` 与实际文件相符；
5. 创建 GitHub Release，附件为 `lazycat-frp-panel-client-<version>.lpk`、上游与客户端源码
   归档、`SHA256SUMS`。

防呆与重跑约定：

- **标签与 `package.yml` 的 `version` 不一致会直接失败**（例如标签 `v0.2.0` 但 package.yml
  还写着 `0.1.0`），避免发出名实不符的包；标签必须形如 `v1.2.3`（可带 `-rc1` 后缀）。
- 同名 Release 已存在时，重跑会以 `--clobber` 覆盖附件，不会因为「已存在」而报错。
- 也可以在 **Actions → Release → Run workflow** 手动触发，填入已推送的标签来重跑发布。

下载已发布的包：

```bash
gh release download v0.2.0 -R fanhuadesenlinnn/lazycat-frp-panel-client -D dist/release
```

本仓库的第一个正式版本 `v0.1.0` 就是由这条流水线发布的（
[Release v0.1.0](https://github.com/fanhuadesenlinnn/lazycat-frp-panel-client/releases/tag/v0.1.0)，
[运行记录 38256147419](https://github.com/fanhuadesenlinnn/lazycat-frp-panel-client/actions/runs/38256147419)），
下载后的 `sha256sum -c SHA256SUMS`、`lzc-cli lpk lint` 均已复验通过。

---

## 故障排查

| 现象 | 可能原因 / 处理 |
| --- | --- |
| 状态一直是「异常退出，等待重启」，日志里报连接失败 | Master 地址/端口不通，或 `--rpc-url` 协议不对（只支持 `grpc/ws/wss`）。界面右侧探针可确认本容器到 Master 的网络是否可达。 |
| 日志里 `client secret cannot be empty` / `client id cannot be empty` | 配置不完整，或粘贴的命令缺少 `-s` / `-i`。重新在 Master 客户端页面复制完整命令。 |
| 页面提示「当前账号不是管理员」 | 默认只允许管理员账号使用管理界面；把 `LZCFRPP_REQUIRE_ADMIN` 改为 `0` 可放开给任意已登录用户。 |
| 页面提示「未检测到登录身份」 | 你是直接访问的管理端口，而不是通过懒猫入口打开。请从懒猫应用入口进入；独立部署请用访问令牌登录。 |
| 改了密钥后客户端没有生效 | 修改会写盘并自动重启正在运行的客户端；若客户端处于停止状态，需要手动点「启动」。 |
| 暴露的服务连不上 | 客户端在容器网络里，`127.0.0.1` 指容器自身。把目标改成宿主机的局域网 IP。 |
| 想让日志更详细 | 把「日志级别」设为 `debug` 并保存，会自动重启客户端生效。 |
| 客户端一直连不上 Master 的 9001 端口 | 上游默认 `CLIENT_TLS_RPC=true`（Master 的 gRPC 端口默认启用 TLS），所以 `grpc://host:9001` 就是正确写法；若你的 Master 关掉了 gRPC TLS，需要给客户端补 `CLIENT_TLS_RPC=false`——本应用界面没有该开关，可在 `lzc-manifest.yml` 的 `environment` 里追加后重新打包。 |
| 粘贴的命令里带了 `CLIENT_FEATURES_*` / `LOGGER_*` 赋值 | 这些赋值会被**忽略**并提示（安全边界与日志级别只由界面控制）；请用界面上的开关与下拉框设置。 |

---

## 已知限制

- 第一版**不包含** WireGuard 组网管理与 Serverless Worker 管理界面；上游客户端本体仍会
  按 Master 下发的配置拉取 WireGuard / Worker 数据，但本应用不做针对性展示与管控。
- 不支持 `--join-token` 自动加入：请先在 Master 的 WebUI 客户端页面创建客户端，
  再复制含 `-s` / `-i` 的启动命令。
- 本应用**无法**从本机确认客户端是否已在 Master 上注册成功，「在线」状态请以 Master 为准。
- 日志缓冲在内存中（默认 800 行），容器重启即清空；不做落盘。
- 跨架构只提供 `linux/amd64` 与 `linux/arm64`。

---

## 许可证与上游来源

- 本应用（管理器 + 网页界面）：**AGPL-3.0**，见 `LICENSE`。
- 上游 `frp-panel`（含随包分发的 `frp-panel-client` 二进制）：**AGPL-3.0**，
  源码 <https://github.com/VaalaCat/frp-panel>，固定 commit
  `1a58b856d7de19de8669b7072872986d2fa1604a`，未做任何修改，
  详见 `content/licenses/NOTICE.txt`。
- 上游以独立子进程方式调用（不链接、不修改），其完整源代码可从上述地址获取。

## 验证状态

已经实际跑通的检查：

- **本地（Windows + Go 1.24.1）**：`gofmt` 无差异、`go vet ./...` 干净、`go test ./...` 全绿；
  并做过本机 HTTP 冒烟测试——`/healthz`、内嵌页面与静态资源、匿名访问 401、
  伪造 `X-HC-*` 请求头被拒（未开启信任时）、缺少自定义请求头返回 403、
  错误令牌 401 / 正确令牌换取 HttpOnly 会话 Cookie、带会话访问成功、退出登录后失效、
  保存配置不回传密钥、日志中密钥已脱敏、开启 `-trust-ingress` 后管理员通过而普通账号被拒、
  以及「非回环监听 + `-trust-ingress`」被拒绝启动。
- **GitHub Actions（Linux）**：[`Build application`](
  https://github.com/fanhuadesenlinnn/lazycat-frp-panel-client/actions/workflows/build.yml)
  在 push `main` 后执行了 `go vet ./...`、`go test -race -count=1 ./...`、
  两个架构（amd64/arm64）的交叉编译、`lzc-cli project release` 打包与 `lzc-cli lpk lint`，
  全部通过；产物 `lazycat-frp-panel-client-0.1.0.lpk`（LPK v2）与上下游源码归档、
  `SHA256SUMS` 一起上传。示例运行：[run 38251077121](
  https://github.com/fanhuadesenlinnn/lazycat-frp-panel-client/actions/runs/38251077121)。
- 下载核对：CI 产物中的 `.lpk` 经 `lzc-cli lpk info` / `lpk lint` 校验通过，
  `content.tar` 内 `start.sh` 与 `bin/**` 的可执行位为 `0755`；两个架构的 ELF 类型与
  `machine` 字段正确（amd64 `0x3E`、arm64 `0xB7`），文件大小与本机交叉编译一致，
  `SHA256SUMS` 与实际文件相符。注意 Go 构建并非逐字节可复现，因此 CI 与本机编译出的
  二进制哈希通常不同，属预期现象。

**尚未**验证的部分（请以你自己的环境实测为准）：

- 未在懒猫微服设备上实机安装、启动过本应用（没有可用的测试设备）。
- 未连接真实远端 Master 做过端到端联调；上游客户端的实际穿透效果取决于你的 Master 配置。
- Docker 镜像只做了 Dockerfile 与运行参数的静态检查，未在容器里实测运行。
