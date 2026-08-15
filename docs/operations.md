# Relay Panel 运维与排障

本页收录日常维护、故障排查和开发信息。第一次部署请先阅读项目根目录的 [README](../README.md)。

## SSH 管理菜单

主控和 Agent 均可执行：

```bash
zf
```

菜单会自动识别机器角色，提供状态、更新、启停、重启、日志、健康检查、连通性检查、密码重置和安全卸载。

## 独立维护命令

### 更新主控

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh | sudo bash
```

详细更新机制见 [UPDATE.md](../UPDATE.md)。

### 更新 Agent

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update-agent.sh | sudo bash
```

### 重置管理员密码

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/reset-admin-password.sh | sudo bash
```

脚本会先备份 SQLite 数据库，再修改密码并重启主控，不影响节点、线路、规则和流量记录。

### 安全卸载

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash
```

脚本会自动识别主控或 Agent，并要求输入大写 `YES`。默认保留可恢复数据，也不会卸载可能被其他程序使用的 Docker、nftables、tc、ping 或 Realm 软件包。

同一台机器同时安装主控和 Agent 时，可以指定目标：

```bash
# 只卸载 Agent
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash -s -- --agent

# 只卸载主控
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash -s -- --control
```

只有确认不再需要数据库、规则、流量记录和备份时，才执行永久清除：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh \
  | sudo bash -s -- --control --purge
```

## 数据备份

“系统设置 → 转发配置备份”可以下载或上传规则 JSON：

- 包含线路、入口/出口引擎、各出口端口池、转发目标和限速。
- 不包含管理员密码、Agent Token、流量统计和探测历史。
- 上传前检查节点、拓扑和端口冲突，通过后按 ID 事务合并。

规则备份引用面板生成的服务器 ID，因此恢复前必须确保对应服务器仍然存在。完整灾难恢复还应备份 Docker volume `relay-data` 中的 SQLite 数据库。

## Agent 无法连接主控

在 Agent 服务器执行：

```bash
sudo systemctl status relay-agent --no-pager
sudo journalctl -u relay-agent -n 80 --no-pager
curl -v --connect-timeout 8 https://你的面板域名/healthz
```

常见结果：

- `Could not resolve host`：DNS 解析问题。
- `Connection refused` 或 `timeout`：端口、防火墙或安全组问题。
- HTTPS 证书错误：检查域名和完整证书链。
- Agent 日志出现 `401`：Node ID 与 Agent Token 不匹配。

不要把 Token 粘贴到公开日志或工单。

## 节点在线但无法转发

按顺序检查：

1. 入口是否能访问线路中填写的出口内网 IP。
2. 出口接入 IP 是否真实存在于该机器。
3. 网卡名是否与 `ip -4 addr show` 和 `ip route` 的结果一致。
4. 云安全组和其他防火墙是否放行入口端口、中继端口和已建立连接。
5. nftables 模式的落地地址是否为 IPv4。
6. Agent 卡片是否显示配置应用错误。

像 CNIX 这类“内部路由可达，但机器没有 `wg0`”的线路，应填写机器实际持有的接入 IP 和实际网卡，例如 `eth0`。

## 安全边界

- Agent 只管理 `inet relay_panel` 表和句柄 `7a1:` 的 qdisc，不刷新其他 nftables 表。
- 如果网卡已经存在根 qdisc，默认拒绝覆盖；确认网卡可由面板独占后才能启用 `allow_qdisc_replace`。
- Agent 每 10 秒与主控对账；主控短暂离线不会删除已经应用的规则。
- Agent 启动或 Realm 异常退出后会检查实际状态并恢复缺失规则。
- nftables 下发前执行语法检查，后续步骤失败时恢复旧表。
- 其他防火墙使用 drop 策略时，仍需自行放行监听端口、中继端口和已建立连接。

## 手工启动与本地开发

准备主控环境变量：

```bash
cp .env.example .env
docker compose up -d --build
```

前端开发：

```bash
npm install
npm run dev
```

另开终端启动 Go 控制端：

```bash
RELAY_ADMIN_PASSWORD='change-this-password' \
RELAY_WEB_URL='http://localhost:3000' \
go run ./cmd/relay-control
```

完整检查：

```bash
make lint
make test
```
