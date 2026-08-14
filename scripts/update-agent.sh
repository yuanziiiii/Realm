#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
version="${RELAY_PANEL_VERSION:-latest}"
config_path="/etc/relay-agent/config.json"
binary_path="/usr/local/bin/relay-agent"
service_name="relay-agent.service"

say() { printf '[Relay Agent] %s\n' "$*"; }
fail() { printf '[Relay Agent] 错误：%s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || fail "Agent 仅支持 Linux"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
command -v systemctl >/dev/null 2>&1 || fail "当前系统不使用 systemd"
command -v curl >/dev/null 2>&1 || fail "缺少 curl"
command -v sha256sum >/dev/null 2>&1 || fail "缺少 sha256sum"
[[ -s "${config_path}" ]] || fail "没有找到现有 Agent 配置 ${config_path}，请先从面板安装 Agent"
systemctl cat "${service_name}" >/dev/null 2>&1 || fail "没有找到 ${service_name}"

if ! command -v ping >/dev/null 2>&1; then
  say "安装线路延迟探测工具"
  probe_package_installed=true
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y && DEBIAN_FRONTEND=noninteractive apt-get install -y iputils-ping || probe_package_installed=false
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y iputils || probe_package_installed=false
  elif command -v yum >/dev/null 2>&1; then
    yum install -y iputils || probe_package_installed=false
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache iputils || probe_package_installed=false
  else
    probe_package_installed=false
  fi
  if [[ "${probe_package_installed}" != true ]]; then
    say "未能安装 ping，将继续更新 Agent；该节点暂时不提供 ICMP 延迟探测"
  fi
fi

case "$(uname -m)" in
  x86_64|amd64) agent_arch="amd64" ;;
  aarch64|arm64) agent_arch="arm64" ;;
  *) fail "暂不支持的 CPU 架构：$(uname -m)" ;;
esac

if [[ "${version}" == "latest" ]]; then
  download_base="https://github.com/${repo}/releases/latest/download"
else
  download_base="https://github.com/${repo}/releases/download/${version}"
fi

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
binary_name="relay-agent-linux-${agent_arch}"

say "下载 Relay Agent ${version} (${agent_arch})"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}" -o "${tmp_dir}/relay-agent"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}.sha256" -o "${tmp_dir}/relay-agent.sha256"
expected="$(awk '{print $1}' "${tmp_dir}/relay-agent.sha256")"
actual="$(sha256sum "${tmp_dir}/relay-agent" | awk '{print $1}')"
[[ -n "${expected}" && "${actual}" == "${expected}" ]] || fail "Agent SHA-256 校验失败"
chmod 0755 "${tmp_dir}/relay-agent"

zf_available=false
if curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/zf.sh" -o "${tmp_dir}/zf" \
  && curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/zf.sh.sha256" -o "${tmp_dir}/zf.sha256"; then
  zf_expected="$(awk '{print $1}' "${tmp_dir}/zf.sha256")"
  zf_actual="$(sha256sum "${tmp_dir}/zf" | awk '{print $1}')"
  [[ -n "${zf_expected}" && "${zf_actual}" == "${zf_expected}" ]] || fail "zf 管理工具的 SHA-256 校验失败"
  chmod 0755 "${tmp_dir}/zf"
  zf_available=true
fi

if [[ -x "${binary_path}" ]]; then
  cp -a "${binary_path}" "${tmp_dir}/relay-agent.previous"
fi

say "保留现有 Node ID、Token 和控制端地址，更新 Agent"
systemctl stop "${service_name}"
install -m 0755 "${tmp_dir}/relay-agent" "${binary_path}"
if systemctl start "${service_name}" && sleep 2 && systemctl is-active --quiet "${service_name}"; then
  [[ "${zf_available}" == true ]] && install -m 0755 "${tmp_dir}/zf" /usr/local/bin/zf
  say "更新完成，原 Agent Token 和配置未改动"
  [[ "${zf_available}" == true ]] && say "SSH 管理工具已更新，输入 zf 即可使用"
  exit 0
fi

if [[ -x "${tmp_dir}/relay-agent.previous" ]]; then
  say "新版启动失败，正在恢复旧版本"
  install -m 0755 "${tmp_dir}/relay-agent.previous" "${binary_path}"
  systemctl restart "${service_name}" || true
fi
journalctl -u "${service_name}" -n 30 --no-pager >&2 || true
fail "Agent 更新失败，已尝试恢复旧版本"
