#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
version="${RELAY_PANEL_VERSION:-latest}"
controller_url="${RELAY_CONTROLLER_URL:-}"
node_id="${RELAY_NODE_ID:-}"
token="${RELAY_AGENT_TOKEN:-}"
install_realm=true

say() { printf '[Relay Agent] %s\n' "$*"; }
fail() { printf '[Relay Agent] 错误：%s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'EOF'
用法：
  curl -fsSL https://raw.githubusercontent.com/yuanziiiii/Realm/main/scripts/install-agent-online.sh \
    | sudo bash -s -- --controller https://面板域名 --node-id SERVER_ID

参数：
  --controller URL    控制端地址
  --node-id ID        面板创建服务器后显示的 Server ID
  --token TOKEN       面板仅显示一次的 Agent Token
  --skip-realm        不安装 Realm（nftables 模式仍可用）
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --controller) controller_url="${2:-}"; shift 2 ;;
    --node-id) node_id="${2:-}"; shift 2 ;;
    --token) token="${2:-}"; shift 2 ;;
    --skip-realm) install_realm=false; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "未知参数：$1" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || fail "Agent 仅支持 Linux"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
command -v systemctl >/dev/null 2>&1 || fail "当前系统不使用 systemd"
if [[ -z "${token}" && -r /dev/tty ]]; then
  printf '请输入面板仅显示一次的 Agent Token：' > /dev/tty
  IFS= read -r -s token < /dev/tty
  printf '\n' > /dev/tty
fi
[[ "${controller_url}" =~ ^https?://[^[:space:]]+$ ]] || fail "请提供有效的 --controller 地址"
[[ -n "${node_id}" ]] || fail "缺少 --node-id"
[[ -n "${token}" ]] || fail "缺少 --token"

install_packages() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl jq kmod nftables iproute2 tar
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl jq kmod nftables iproute tar
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl jq kmod nftables iproute tar
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl jq kmod nftables iproute2 tar
  else
    fail "不支持当前包管理器"
  fi
}

case "$(uname -m)" in
  x86_64|amd64) agent_arch="amd64"; realm_target="x86_64-unknown-linux-musl" ;;
  aarch64|arm64) agent_arch="arm64"; realm_target="aarch64-unknown-linux-musl" ;;
  *) fail "暂不支持的 CPU 架构：$(uname -m)" ;;
esac

install_packages
tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

if [[ "${version}" == "latest" ]]; then
  download_base="https://github.com/${repo}/releases/latest/download"
else
  download_base="https://github.com/${repo}/releases/download/${version}"
fi
binary_name="relay-agent-linux-${agent_arch}"
say "下载 Relay Agent ${version} (${agent_arch})"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}" -o "${tmp_dir}/relay-agent"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}.sha256" -o "${tmp_dir}/relay-agent.sha256"
expected="$(awk '{print $1}' "${tmp_dir}/relay-agent.sha256")"
actual="$(sha256sum "${tmp_dir}/relay-agent" | awk '{print $1}')"
[[ "${actual}" == "${expected}" ]] || fail "Agent SHA-256 校验失败"
install -m 0755 "${tmp_dir}/relay-agent" /usr/local/bin/relay-agent

if [[ "${install_realm}" == true ]]; then
  say "安装 Realm"
  realm_archive="realm-${realm_target}.tar.gz"
  curl --proto '=https' --tlsv1.2 -fsSL "https://github.com/zhboner/realm/releases/latest/download/${realm_archive}" -o "${tmp_dir}/realm.tar.gz"
  tar -xzf "${tmp_dir}/realm.tar.gz" -C "${tmp_dir}"
  realm_binary="$(find "${tmp_dir}" -type f -name realm | head -n 1)"
  [[ -n "${realm_binary}" ]] || fail "Realm 压缩包中未找到可执行文件"
  install -m 0755 "${realm_binary}" /usr/local/bin/realm
fi

install -d -m 0700 /etc/relay-agent /var/lib/relay-agent
jq -n \
  --arg controller_url "${controller_url%/}" \
  --arg node_id "${node_id}" \
  --arg token "${token}" \
  '{controller_url:$controller_url,node_id:$node_id,token:$token,apply:true,allow_qdisc_replace:false,sync_interval:"10s",state_dir:"/var/lib/relay-agent",realm_binary:"/usr/local/bin/realm"}' \
  > /etc/relay-agent/config.json
chmod 0600 /etc/relay-agent/config.json

cat > /etc/systemd/system/relay-agent.service <<'EOF'
[Unit]
Description=Relay Panel node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/relay-agent -config /etc/relay-agent/config.json
Restart=always
RestartSec=3
NoNewPrivileges=true
PrivateTmp=true
ProtectHome=true
ProtectSystem=strict
ReadWritePaths=/var/lib/relay-agent
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF
printf 'net.ipv4.ip_forward = 1\n' > /etc/sysctl.d/99-relay-panel.conf
sysctl --system >/dev/null
systemctl daemon-reload
systemctl enable --now relay-agent
sleep 2
systemctl is-active --quiet relay-agent || {
  journalctl -u relay-agent -n 30 --no-pager >&2
  fail "Agent 启动失败"
}
say "安装完成，Agent 已连接 ${controller_url%/}"
