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

更新器会校验下载文件，构建候选镜像并检查网页端和控制端健康状态。新版启动失败时会自动恢复原镜像。

## 更新 Agent

在对应的入口或出口服务器执行：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update-agent.sh | sudo bash
```

Agent 更新会保留 `/etc/relay-agent/config.json` 中的主控地址、Node ID 和 Token，失败时恢复旧二进制。

## 指定版本

需要固定版本时，可以通过环境变量指定，例如：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh \
  | sudo env RELAY_PANEL_VERSION=v0.3.9 bash
```

数据库会自动迁移。升级前仍建议备份 `/opt/relay-panel/data` 数据目录。
