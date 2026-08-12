# Relay Panel

面向个人自用的多服务器端口转发面板。控制端支持两种接管模式：

```text
双端托管：本机 → 广州公网入口（Agent）→ 内网/专线 → 香港出口（Agent）→ 落地
仅出口接管：本机 → 广州已有转发 → 香港内网接入（Agent）→ 落地
```

核心目标是把规则、安全下发、上下行限速和流量统计放在一个小而清晰的系统里，不包含注册、套餐、支付、工单或多租户功能。

## 当前能力

- 单管理员登录，SQLite 本地持久化
- 广州入口、香港出口服务器和 Agent 在线状态
- 双端托管、仅出口接管两种规则模式
- TCP、UDP、TCP+UDP 规则
- nftables 内核转发和 Realm 进程转发
- 每条规则独立上传、下载限速，使用 `nftables mark + tc HTB + fq_codel`
- Realm 入站整形使用 IFB
- nftables named counter 统计上传/下载字节与包数
- 最近 24 小时、今日和规则维度的流量聚合
- Agent 每 10 秒对账；控制端短暂离线不会删除已应用规则
- nftables 应用前语法检查，后续步骤失败时恢复旧表
- Agent Token 只在创建节点时返回一次，数据库仅保存 SHA-256 摘要

## 组件

```text
浏览器 ──HTTPS──> relay-control ──反向代理──> Web UI
                       │
                       ├── SQLite
                       │
                       └<──HTTPS── relay-agent（主动拉取期望状态）
                                      ├── nftables
                                      ├── tc / IFB
                                      └── Realm（可选）
```

控制端由 Go 编写；管理界面使用 React。Agent 必须运行在 Linux，并需要 `CAP_NET_ADMIN`。推荐 Debian 12 或 Ubuntu 22.04/24.04。

## 快速启动控制端

### 一键安装（推荐）

在一台全新的 Linux 控制端服务器运行：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-control.sh | sudo bash
```

脚本会安装 Docker、下载项目、生成随机管理员密码并启动控制端。完成时会打印面板地址和密码。默认安装到 `/opt/relay-panel`，SQLite 数据保存在 Docker volume 中。

### 手工安装

1. 准备环境变量：

   ```bash
   cp .env.example .env
   ```

   修改 `.env` 中的管理员密码和随机 Session Secret。生产环境由 HTTPS 反向代理访问时，将 `RELAY_SECURE_COOKIES` 改为 `true`。

2. 启动：

   ```bash
   docker compose up -d --build
   ```

3. 打开 `http://控制端IP:8080`。在“服务器”页创建需要被面板接管的服务器，创建结果会显示 Server ID 与一次性 Agent Token。

## 安装节点 Agent

先在面板“服务器”页添加广州入口或香港出口，复制只显示一次的 Server ID 和 Agent Token，然后在该服务器运行：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-agent-online.sh \
  | sudo bash -s -- \
      --controller https://你的面板域名 \
      --node-id 面板显示的ServerID
```

脚本随后会在终端中隐式询问 Agent Token，避免 Token 留在 shell 历史。安装器支持 Debian、Ubuntu、Alpine、RHEL 系发行版，以及 x86_64、ARM64；自动安装 nftables、`tc`、systemd 服务和 Realm。只使用 nftables 时可添加 `--skip-realm`。

### 手工安装

在仓库根目录构建 Linux Agent，或在 Linux 节点本机构建：

```bash
go build -trimpath -o relay-agent ./cmd/relay-agent
```

复制示例配置并填写面板显示的 `node_id`、`token` 和控制端 HTTPS 地址：

```bash
cp deploy/relay-agent.example.json node.json
sudo ./scripts/install-agent.sh ./relay-agent ./node.json
```

广州入口的 `public_interface` 是客户端流量进入/返回的公网网卡，`private_interface` 是前往香港出口的内网、WireGuard 或专线网卡。香港出口的 `private_interface` 接收广州方向的流量，`public_interface` 用于访问落地。

Realm 模式还需将 Realm 二进制安装到 `/usr/local/bin/realm`，或者修改 Agent 配置中的 `realm_binary`。

## 两种转发模式

### 1. 双端托管

广州入口和香港出口都安装 Agent。面板为同一条规则分别生成并下发两端配置：

```text
客户端 → 广州公网 IP:24444
       → 香港内网 IP:自动中继端口
       → 香港公网出口 → 落地 IP:端口
```

在面板选择：

```text
模式：双端托管
广州入口服务器：已安装 Agent 的广州机器
香港出口服务器：已安装 Agent 的香港机器
转发引擎：nftables / Realm
广州公网端口：24444
落地 IP 和端口
```

面板自动分配内网中继端口。你不需要再登录广州或香港机器手工创建该条转发规则。

### 2. 仅出口接管

广州到香港的内网转发已经由你预先配置，只在香港出口安装 Agent。面板不会管理第一跳，只管理香港内网接入到落地这一段：

```text
客户端 → 广州已有规则
       → 香港内网 IP:24444
       → 香港公网出口 → 落地 IP:端口
```

在面板选择：

```text
模式：仅出口接管
香港出口服务器：已安装 Agent 的香港机器
香港内网接入 IP：通常自动使用该机器的内网 IP
香港内网接入端口：必须与广州已有规则的目标端口一致
转发引擎：nftables / Realm
落地 IP 和端口
```

此模式下广州入口不需要安装 Agent，也不会收到任何面板配置。

## 速率控制和流量统计

两种模式都可以在规则的“高级设置”中填写：

```text
上传限速：客户端发往落地的方向，0 表示不限速
下载限速：落地返回客户端的方向，0 表示不限速
Burst：HTB 允许的短时突发，默认 512 KB
```

双端托管在广州入口侧计量；仅出口接管在香港内网接入侧计量。每条链路只累计一次，避免双机重复。Agent 上报内核累计快照，控制端通过基线计算增量；重复请求不会重复累计，计数器重置也能识别。

## 安全与系统约束

- nftables 模式当前以 IPv4 为主，落地地址必须填写 IPv4；Realm 模式可使用域名或 IPv6。
- Agent 安装脚本启用 `net.ipv4.ip_forward=1`。
- `allow_qdisc_replace` 默认为 `false`。如果网卡已有其他根 qdisc，Agent 会停止应用限速，防止覆盖现有 QoS；确认该网卡可由面板独占后才能改为 `true`。
- Agent 只管理 `inet relay_panel` 表和句柄 `7a1:` 的 qdisc，不刷新系统其他 nftables 表。
- 其他防火墙链若使用 drop 策略，仍需放行监听端口、内网中继端口和已建立连接。
- 控制端必须使用 HTTPS 暴露给异地 Agent；不要通过明文公网 HTTP 发送 Agent Token。
- SQLite 数据位于 Docker volume `relay-data`。备份时可停止控制端并复制该 volume 中的 `relay-panel.db`。

## 本地开发与验证

前端：

```bash
npm install
npm run dev
```

控制端：

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

在非 Linux 系统可以把 Agent 的 `apply` 设为 `false`，用于验证同步和配置生成，但不会修改网络规则。

## MVP 边界

当前版本适合个人部署和小规模节点。尚未实现 IPv6 nftables NAT、多出口健康检查、规则编辑表单、WebSSH、通知告警和自动升级。这些可以在基础转发通过真实服务器联调后继续迭代。

项目采用 MIT License。Realm 本身遵循其上游项目许可证。
