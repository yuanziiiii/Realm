# Relay Panel

面向个人自用的多服务器端口转发面板。控制端采用“服务器 → 线路 → 转发规则”三层模型：

```text
服务器：接入 Agent、显示状态和网络参数
线路：固定入口、出口、接管模式，并分别选择入口 / 出口引擎
转发规则：选择线路后，只填写协议、监听端口、落地地址和限速
```

线路支持两种接管模式，不绑定任何地区或服务商：

```text
双端托管：本机 → 公网入口（Agent）→ 内网/专线 → 出口（Agent）→ 落地
仅出口接管：本机 → 已有入口转发 → 出口内网接入（Agent）→ 落地
```

核心目标是把规则、安全下发、上下行限速和流量统计放在一个小而清晰的系统里，不包含注册、套餐、支付、工单或多租户功能。

## 当前能力

- 单管理员登录，SQLite 本地持久化
- 服务器一键接入、Agent 在线状态和网络参数维护
- 可复用线路，支持双端托管、仅出口接管两种拓扑
- 双端托管支持一个主出口和多个备用出口；入口 Agent 按出口内网 IP 上报延迟、丢包和在线状态
- 出口 Agent 自动检查“出口 → 落地”：全部规则上报 ICMP 延迟和丢包，TCP / TCP+UDP 规则额外检查真实业务端口的 TCP 握手状态；探测共享最多 8 个并发，不发送业务数据
- 可选自动故障切换：连续 3 次失败后切到健康备用出口，主出口连续 3 次恢复后按优先级回切
- 双端线路的入口、出口引擎可独立选择，支持 nftables → Realm、Realm → nftables 等混合组合；仅出口接管只配置出口引擎
- 线路可修改入口、出口、接入 IP、NAT 端口池和两段转发引擎，已有规则自动迁移并重新下发
- 出口 NAT 机器支持端口或端口范围，例如 `20000-20999,25000`
- Agent 首次上线自动回填公网 IP、默认出口网卡、内网 IP 和内网网卡；面板手工配置优先且不会被覆盖
- 修改服务器的地址、网卡或用途后会提升配置版本，受影响的 nftables、Realm 和 tc 规则由 Agent 自动重新生成并下发
- 已安装 Agent 可从服务器卡片复制免 Token 更新命令；更新保留原 Node ID、Token 和控制端地址，失败自动回滚
- 转发规则按线路分组，日常添加不再重复选择服务器和入口 / 出口引擎
- 已有转发可修改所属线路、协议、监听端口、落地地址和速率限制，保存后自动重新下发
- TCP、UDP、TCP+UDP 规则
- nftables 内核转发和 Realm 进程转发
- 每条规则独立上传、下载限速，使用 `nftables mark + tc HTB + fq_codel`
- Realm 入站整形使用 IFB
- nftables named counter 统计上传/下载字节与包数
- 今日、本周、本月、本季度和规则维度的流量聚合
- 流量趋势支持平滑折线图和并列柱状图切换，包含动态纵轴、北京时间横轴及悬浮上传/下载明细
- “流量统计”列表中的每条规则均可直接点击，独立查看该规则今日、本周、本月和本季度的折线图或柱形图
- 实时速率只在流量页面或规则详情打开时每 10 秒刷新
- 分钟明细保留 7 天，长期数据按日汇总，避免数据库随运行时间快速增长
- 页面支持浅色、深色和跟随系统三种主题；选择会保存在当前浏览器，登录页和管理控制台保持一致
- Agent 每 10 秒对账；控制端短暂离线不会删除已应用规则
- Agent 启动或 Realm 异常退出后会核对 nftables、tc 和 Realm 实际状态，缺失时自动恢复，不只依赖旧 revision
- nftables 应用前语法检查，后续步骤失败时恢复旧表
- Agent 下发失败会在服务器卡片直接显示原因
- Agent Token 只在创建节点时返回一次，数据库仅保存 SHA-256 摘要
- “系统设置”可下载和上传线路与转发规则 JSON；上传先预检节点、拓扑与端口冲突，再按 ID 事务合并
- 主控和 Agent 安装后提供 `zf` SSH 交互管理菜单，并按机器角色显示对应维护功能

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

脚本会安装 Docker、下载项目、生成随机管理员密码并启动控制端。系统没有默认用户名，只使用脚本生成的管理员密码登录；完成时会打印面板地址和密码，请立即保存。

默认监听端口为 `8080`，安装时可以固定为其他端口，例如 `18080`：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-control.sh \
  | sudo env RELAY_HTTP_PORT=18080 bash
```

默认安装到 `/opt/relay-panel`，SQLite 数据保存在 Docker volume 中。安装完成后需要修改面板端口时，编辑 `/opt/relay-panel/.env` 中的 `RELAY_HTTP_PORT`，再运行：

```bash
sudo docker compose --project-directory /opt/relay-panel up -d
```

安装脚本只用于全新安装；检测到 `/opt/relay-panel` 已存在时会主动停止，不会覆盖已有数据库或配置。

### 更新控制端

更新命令会先比较本机记录版本与 GitHub 最新稳定版；版本相同时直接退出，不下载大文件，也不重建容器。发现新版本后会下载 GitHub Release 已预构建的对应 CPU 架构镜像，同时更新网页端和 Go 控制端，不修改数据库、登录密码、端口或 HTTPS 反代配置。更新器会做健康检查，失败时同时恢复旧网页端与旧控制端：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update.sh | sudo bash
```

全新安装与在线更新已经分开；已经部署过主控后只需执行上述更新命令，不要重新运行安装脚本。完整说明见 [UPDATE.md](UPDATE.md)。面板“系统设置”页面也会固定显示并提供复制这条更新命令。

从支持增量更新的版本开始，更新时不再在服务器执行 `npm install`、前端构建或 Go 编译，只载入预构建镜像并短暂重建两个容器。没有本地版本记录的安装第一次使用新更新器时，会正常更新一次并写入 `/opt/relay-panel/.relay-panel-version`；以后再次检查相同版本会立即结束。预构建镜像的下载量会大于源码包，但能显著降低服务器的 CPU、内存和 Swap 峰值，临时下载文件会在结束时自动清理。

### 备份和恢复转发配置

打开“系统设置 → 转发配置备份”，点击“下载配置文件”即可导出线路、入口/出口引擎、NAT 端口池、转发目标和限速。配置文件不包含管理员密码、Agent Token、流量统计或探测历史。

上传 JSON 时，面板先检查所引用的服务器是否仍存在、线路是否完整以及入口/中继端口是否冲突；全部通过后才会在一个数据库事务内按 ID 合并。同 ID 的线路或规则会更新，新 ID 会新增，未包含在文件中的现有配置不会删除。任何一项写入失败都会整体回滚。

规则引用的是面板生成的服务器 ID，因此恢复前必须保证对应服务器仍存在于当前面板；缺少服务器时面板会列出错误并拒绝写入。此功能用于日常保存和迁移转发配置，完整灾难恢复仍应备份 SQLite 数据卷。

### 忘记或重置管理员密码

在控制端 SSH 终端执行一条命令，按提示输入两遍新密码即可。脚本会自动备份 SQLite 数据库、更新密码并重启控制端，不影响服务器、线路、规则和流量记录：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/reset-admin-password.sh | sudo bash
```

### SSH 交互管理菜单

安装或更新主控、Agent 后，在对应服务器 SSH 中直接输入：

```bash
zf
```

工具会自动识别当前机器角色。存在有效 Agent 配置和 `relay-agent.service` 时只显示被控端菜单，不会因残留的主控目录误显示主控功能；主控端则需要完整的 Docker Compose 与环境配置。主控菜单包含状态、在线更新、启停/重启、日志、健康检查、密码重置和访问配置；Agent 菜单包含状态、在线更新、启停/重启、日志、主控连通性、节点信息和转发环境检查。Agent Token 只显示“已配置/缺失”，不会输出明文。

停止服务和密码重置均需要二次确认。主控端和 Agent 端菜单都提供 `10. 安全卸载`：选择后会下载并校验正式版卸载脚本，仍需输入大写 `YES` 才会执行，不提供容易误触的免确认卸载。

### 卸载

可以直接运行 `zf` 并选择 `10. 安全卸载`，也可以执行下面的独立命令。脚本会自动识别当前机器是主控还是 Agent，并要求输入 `YES`。默认卸载会保留可恢复数据：Agent 配置备份到 `/var/backups/relay-panel/`；主控目录移动为 `/opt/relay-panel.backup-时间`，Docker 数据卷不会删除。

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash
```

如果同一台机器同时装有主控和 Agent，需要明确选择：

```bash
# 只卸载 Agent
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash -s -- --agent

# 只卸载主控
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash -s -- --control
```

只有确认不再需要数据库、规则、流量记录和备份时，才使用永久清除：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/uninstall.sh | sudo bash -s -- --control --purge
```

卸载不会移除 Docker、nftables、`tc`、`ping` 或 Realm 软件包，因为它们可能同时被其他服务使用。

### 1C1G 低内存服务器

1 核 1 GB 可以运行个人面板。实机中控制端与网页端两个容器空闲时合计约 140 MB，实际占用会随规则数量和访问量变化。

只有全新安装和手工源码构建需要更多瞬时内存。安装器在物理内存低于 1.5 GB 且现有 Swap 不足时，会创建持久化的 2 GB `/var/lib/relay-panel/build.swap` 并串行构建。日后的在线更新直接载入 Release 预构建镜像，不再使用这块 Swap 进行编译。

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

3. 打开 `http://控制端IP:RELAY_HTTP_PORT`，按页面顺序完成“接入服务器 → 创建线路 → 新建转发”。

## 安装节点 Agent

先在面板“服务器”页添加入口服务器或出口服务器。服务器只是可复用的 Agent 节点，不等于一条线路；同一出口可以被多条线路复用。面板会直接生成一键安装命令和只显示一次的 Agent Token，在对应服务器执行命令并按提示粘贴 Token：

```bash
curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-agent-online.sh \
  | sudo bash -s -- \
      --controller https://你的面板域名 \
      --node-id 面板显示的ServerID
```

脚本随后会在终端中隐式询问 Agent Token，避免 Token 留在 shell 历史。正式安装前，脚本会先检查主控健康接口并用 Node ID、Token 完成一次真实鉴权；域名解析、端口/安全组、HTTPS 证书、反向代理路径或 Token 有问题时会直接指出原因。安装器支持 Debian、Ubuntu、Alpine、RHEL 系发行版，以及 x86_64、ARM64；自动安装 nftables、`tc`、`ping`、systemd 服务和 Realm。只使用 nftables 时可添加 `--skip-realm`。

Agent 是主动连接主控，不需要在被控服务器开放 Agent 入站端口。直连 `http://主控IP:18080` 时，需要在主控服务器和云安全组放行 `18080/TCP`；使用 `https://面板域名` 反代时，外部通常只需放行 `443/TCP`，但反代必须同时覆盖网页、`/healthz` 和 `/agent/v1/sync`，不能只代理网页。

如果已安装但面板仍显示“等待连接”，在被控服务器检查：

```bash
sudo systemctl status relay-agent --no-pager
sudo journalctl -u relay-agent -n 80 --no-pager
curl -v --connect-timeout 8 https://你的面板域名/healthz
```

其中 `Could not resolve host` 是 DNS 问题，`Connection refused/timeout` 通常是监听端口、安全组或防火墙问题，证书报错是 HTTPS 证书链问题，日志中的 `401` 则是 Node ID 与 Agent Token 不匹配。不要把 Token 粘贴到公开日志或工单。

### 更新已安装的 Agent

首次安装后不需要保存面板显示的一次性 Token。打开“服务器”页，在对应服务器卡片点击“更新 Agent”并复制命令，或直接在该服务器执行：

```bash
curl -fsSL https://github.com/yuanziiiii/Realm/releases/latest/download/update-agent.sh | sudo bash
```

更新脚本只替换 Agent 程序，保留 `/etc/relay-agent/config.json` 中现有的 Node ID、Token 和控制端地址，不会再次索取 Token。新版本启动失败时会恢复原程序并重新启动服务。

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

入口的 `public_interface` 是客户端流量进入/返回的公网网卡，`private_interface` 是前往出口的内网、WireGuard 或专线网卡。出口的 `private_interface` 接收入口方向的流量，`public_interface` 用于访问落地。

这些字段不是备注：nftables 会用地址和网卡匹配入站、出站流量，Realm 也会使用配置的监听地址。像 CNIX 这类服务商内部路由可达、但机器本身没有 `wg0` 和专用内网地址的场景，应把出口接入 IP 填为机器实际持有的地址，把接入网卡填为实际网卡（例如 `eth0`）。保存服务器配置后，控制端会自动触发相关 Agent 重新对账，无需再修改线路制造一次下发。

Realm 模式还需将 Realm 二进制安装到 `/usr/local/bin/realm`，或者修改 Agent 配置中的 `realm_binary`。

## 线路与两种接管模式

服务器 Agent 上线后，先创建一条线路。服务器关系以及入口、出口两段引擎只在线路中配置一次；以后新增转发只选择线路并填写端口、落地 IP、落地端口和可选限速。旧版本创建的线路升级后会把原“线路引擎”自动继承到入口和出口，不会中断已有规则。

### 1. 双端托管

入口和出口都安装 Agent。面板为同一条规则分别生成并下发两端配置：

```text
客户端 → 入口公网 IP:24444
       → 出口内网 IP:自动中继端口
       → 出口公网 → 落地 IP:端口
```

创建线路时选择：

```text
模式：双端托管
入口服务器：已安装 Agent 的入口机器
主出口服务器：已安装 Agent 的首选出口机器
备用出口：可选择多个，并按选择顺序作为切换优先级
自动故障切换：可选；入口 Agent 连续探测后自动切换并回切
所有出口共同可用的中继端口：NAT 机器可填 20000-20999,25000；留空不限制
入口引擎：nftables / Realm（入口公网端口 → 出口内网 IP:中继端口）
出口引擎：nftables / Realm（出口中继端口 → 落地 IP:端口）
```

随后在这条线路中新增转发，填写入口公网端口和落地 IP/端口。入口 Agent 只执行入口引擎，出口 Agent 只执行出口引擎；两者互不绑定。面板自动从共同端口池分配内网中继端口，并提前把出口段下发到全部主备出口；发生切换时只需更新入口的目标出口。入口监听端口仍可使用 1-65535。所有主备 NAT 出口必须都能使用该线路配置的中继端口；若两个线路共享同一备用出口，面板也会检查并避开端口冲突。

延迟和丢包由入口 Agent 对每个出口的内网 IP 进行 ICMP 探测。某些服务商会屏蔽 ICMP：此时面板仍以 Agent 心跳判断服务器在线，但不会把一条从未成功回应 ICMP 的路径当作可自动切换的备用线路，从而避免误切换。

每个实际出口 Agent 还会自动探测已下发规则的落地。所有协议都保留 ICMP 网络延迟和丢包；TCP 与 TCP+UDP 规则会再连接真实落地端口，面板显示“TCP 正常、连接超时、拒绝连接、网络不可达”等状态、握手耗时和北京时间最后检查时间。TCP 探测只完成握手并立即断开，不发送业务数据；TCP+UDP 的结果只代表 TCP 部分，UDP-only 仍需用实际业务验证。双端托管中的“入口 → 出口”继续按出口服务器的内网 IP 探测，两段数据互不混用。

### 2. 仅出口接管

入口到出口的转发已经由你预先配置，只在出口安装 Agent。面板不会管理第一跳，只管理出口接入到落地这一段：

```text
客户端 → 已有入口规则
       → 出口内网 IP:24444
       → 出口公网 → 落地 IP:端口
```

创建线路时选择：

```text
模式：仅出口接管
出口服务器：已安装 Agent 的出口机器
出口内网接入 IP：通常自动使用该机器的内网 IP
出口可用中继端口：NAT 机器填写服务商分配的端口或端口范围
出口引擎：nftables / Realm
```

随后在这条线路中新增转发，监听端口必须与已有入口规则的目标端口一致，并且位于配置的出口端口池内，再填写落地 IP/端口。此模式下入口不需要安装 Agent，也不会收到任何面板配置。

已有线路可以在“线路”页点击“修改线路”更换入口、出口、接入 IP、端口池或引擎。保存后，线路下全部规则会同步迁移并重新下发；若新机器存在端口冲突，面板会拒绝保存并指出冲突端口。

## 速率控制和流量统计

两种模式都可以在规则的“高级设置”中填写：

```text
上传限速：客户端发往落地的方向，0 表示不限速
下载限速：落地返回客户端的方向，0 表示不限速
Burst：HTB 允许的短时突发，默认 512 KB
```

双端托管在入口侧计量；仅出口接管在出口内网接入侧计量。每条链路只累计一次，避免双机重复。Agent 上报内核累计快照，控制端通过基线计算增量；重复请求不会重复累计，计数器重置也能识别。

面板按北京时间（Asia/Shanghai）自然周期展示今日、本周、本月和本季度。本周从周一开始，本季度按自然季度计算。全规则趋势和单条规则趋势都可以切换平滑折线图或并列柱形图；在“流量统计”列表点击任意规则，即可进入该规则的四个周期统计和独立趋势。实时速率来自当前分钟内的计数增量，只在流量统计页面或规则流量详情打开时每 10 秒刷新；离开这些页面后不再发起实时查询，Agent 的规则对账不受影响。

SQLite 只保留最近 7 天的分钟明细，同时长期保存每日汇总。每日汇总每条规则每个计量节点每天只有一行，即使保留多年，个人规模部署通常也只是数 MB 到几十 MB；实际大小取决于规则数量和保留年限。升级时会保留原有每日总量并把汇总桶切换为北京时间；已有分钟数据也会自动回填，不会清空历史流量。

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

当前版本适合个人部署和小规模节点。尚未实现 IPv6 nftables NAT、WebSSH、通知告警和全自动无人工升级。

项目采用 MIT License。Realm 本身遵循其上游项目许可证。
