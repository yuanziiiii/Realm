#!/usr/bin/env bash
set -euo pipefail

if [[ "${EUID}" -ne 0 ]]; then
  echo "请使用 root 运行此脚本。" >&2
  exit 1
fi

agent_binary="${1:-./relay-agent}"
agent_config="${2:-./deploy/relay-agent.example.json}"

if [[ ! -f "${agent_binary}" || ! -f "${agent_config}" ]]; then
  echo "用法: sudo ./scripts/install-agent.sh <relay-agent二进制> <config.json>" >&2
  exit 1
fi

install -D -m 0755 "${agent_binary}" /usr/local/bin/relay-agent
install -D -m 0600 "${agent_config}" /etc/relay-agent/config.json
install -D -m 0644 ./deploy/relay-agent.service /etc/systemd/system/relay-agent.service
if [[ -f ./scripts/zf.sh ]]; then
  install -D -m 0755 ./scripts/zf.sh /usr/local/bin/zf
fi
install -D -m 0644 ./deploy/99-relay-panel.conf /etc/sysctl.d/99-relay-panel.conf
install -d -m 0700 /var/lib/relay-agent

sysctl --system >/dev/null
systemctl daemon-reload
systemctl enable --now relay-agent
systemctl --no-pager --full status relay-agent
