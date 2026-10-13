# 懒猫商店提交说明

当前商店包版本为 **0.2.3**，使用用户已转存的镜像：

```text
registry.lazycat.cloud/u2982125700/fanhuadesenlinnn/lazycat-frp-panel-client:716bbc086cce38fa
```

这是包装配置更新，复用现有镜像，不会重新编译并覆盖懒猫仓库镜像。
镜像仅供微服访问，本机与 GitHub 不拉取它；镜像实际可达性由设备安装验证。

## 本地打包与检查

在仓库根目录执行（Node.js 与 lzc-cli 2.0.9+）：

```powershell
npm install -g @lazycatcloud/lzc-cli@2.0.9
New-Item -ItemType Directory -Force dist
lzc-cli project release -f lzc-build.yml -o dist/frp-panel-client-store.lpk
lzc-cli lpk info dist/frp-panel-client-store.lpk
lzc-cli lpk lint dist/frp-panel-client-store.lpk
tar -xOf dist/frp-panel-client-store.lpk manifest.yml
```

确认包内 image 与上述引用一致，并把新 LPK 安装到微服确认页面、保存配置与连接可用。
包内无需 content.tar，也不需要本机拥有 Docker 或编译好的 Go 二进制。

## 可用于提交的介绍

**名称：** frp-panel 客户端

**简介：** 在懒猫微服上连接自建 frp-panel，管理内网穿透客户端。

**详细介绍：** 提供中文管理界面，支持粘贴远端面板生成的客户端启动命令，
保存连接参数、启停与重启客户端、查看进程状态和脱敏日志。
配置持久化保存，支持后台运行。需要用户自行部署远端 frp-panel Master 和 Server；
本应用不提供公共穿透服务器，也不包含远端管理面板。

**使用步骤：** 在远端 Master 创建客户端节点，复制启动命令；使用懒猫管理员账号
打开本应用，粘贴并保存连接信息，再到远端面板配置穿透规则。
访问宿主机或其他局域网设备时，目标地址应使用可达的局域网 IP。

**权限说明：** net.internet 用于连接远端服务器；net.lan 用于访问待穿透的局域网服务。

**许可证：** AGPL-3.0；源代码和上游信息见仓库 LICENSE 与 content/licenses/NOTICE.txt。

## 由维护者提交审核

1. 通过官方开发者中心完成开发者资格申请。
2. 使用仓库 icon.png，并补充自己设备上的真实截图：配置页、运行状态和日志页。
   截图隐藏密钥、客户端 ID 和不希望公开的服务器地址，不使用虚构运行截图。
3. 完成设备安装验证后执行：

```powershell
lzc-cli appstore login
lzc-cli appstore publish dist/frp-panel-client-store.lpk
```

提交后等待审核；本仓库的 CI 不自动提交商店审核。

## GitHub 发布与后续镜像更新

- push main 会检查代码、打包 LPK、验证源码构建镜像并上传 Actions artifact。
- v* 标签发布时，如果 manifest 引用懒猫源，仅发布 LPK 与源码附件，跳过镜像推送。
- 后续修改程序时，先显式构建并发布新的 GHCR 镜像，再运行 `lzc-cli appstore copy-image`
  转存。把返回的新引用写入 manifest，然后发布新的包装版本。不能只改代码而继续沿用旧镜像。
- 镜像构建脚本拒绝向 registry.lazycat.cloud 推送，避免覆盖已有转存标签。

官方资料：[发布流程](https://developer.lazycat.cloud/publish-app.html) ·
[审核指南](https://developer.lazycat.cloud/store-submission-guide.html)
