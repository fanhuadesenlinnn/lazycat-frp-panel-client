# 懒猫 frp-panel 客户端

基于 [frp-panel](https://github.com/VaalaCat/frp-panel) 的懒猫微服客户端，用于连接自建的远端 Master 和 Server，将微服或局域网中的服务通过 FRP 暴露出去。

提供中文管理界面，支持粘贴启动命令、保存连接配置、启动与重启客户端、查看运行状态和脱敏日志。配置持久保存，客户端支持后台运行。

使用前，在远端 Master 创建客户端节点，复制启动命令，粘贴到本应用并点击“解析、保存并启动”。

## 界面介绍

![frp-panel 客户端管理界面](docs/images/client-overview.png)

上方显示客户端运行状态和 Master 可达性；下方可填写连接参数，或粘贴远端面板生成的启动命令。
