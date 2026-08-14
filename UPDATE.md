# Relay Panel 在线更新

在线更新与全新安装是两套独立流程。已经安装过主控后，不要再次执行 `install-control.sh`。

## 更新主控

在主控服务器执行：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh | sudo bash
```

更新内容包括网页端、Go 控制端和数据库兼容层。以下内容会保留：

- SQLite 数据库及流量记录
- 管理员密码和登录配置
- 面板监听端口与 HTTPS 反向代理
- 服务器、线路和转发规则
- Agent Token 与节点关系

更新器先读取 `/opt/relay-panel/.relay-panel-version`，再解析 GitHub 最新稳定版：

- 版本相同：立即结束，不下载镜像、不停止服务、不重建容器
- 发现新版本：下载与服务器 CPU 架构匹配的预构建镜像，不在服务器执行 `npm install`、前端构建或 Go 编译
- 旧安装没有版本文件：执行一次正常更新并自动补写版本，之后即可快速比较

所有下载文件都进行 SHA-256 校验。载入镜像后只重建 `web` 和 `control` 两个容器并运行健康检查；新版启动失败时会自动恢复原镜像。

更新完成后会同时安装或刷新 `/usr/local/bin/zf`。以后登录主控 SSH，直接输入 `zf` 即可通过交互菜单完成更新、启停、日志检查、健康检查和密码重置。

## 更新 Agent

在对应的入口或出口服务器执行：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update-agent.sh | sudo bash
```

Agent 更新会保留 `/etc/relay-agent/config.json` 中的主控地址、Node ID 和 Token，失败时恢复旧二进制。

涉及 Agent 新能力（例如出口到落地的延迟探测）时，应先更新主控，再更新相关入口和出口 Agent。旧 Agent 与新主控保持兼容，但只有升级后的出口 Agent 才会上报新的落地探测数据。

Agent 更新也会安装或刷新同一个 `zf` 命令，但菜单会自动切换为 Agent 状态、日志、连通性和转发环境检查等节点功能。

## 指定版本

需要固定版本时，可以通过环境变量指定，例如：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh \
  | sudo env RELAY_PANEL_VERSION=v0.3.10 bash
```

数据库会自动迁移。升级前仍建议备份控制端容器挂载到 `/data` 的 Docker volume。

排障时如需强制重新载入当前版本，可显式使用：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh \
  | sudo env RELAY_PANEL_FORCE_UPDATE=1 bash
```
