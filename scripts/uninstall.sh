#!/usr/bin/env bash
set -euo pipefail

install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"
role=""
purge=false
assume_yes=false

say() { printf '[Relay Panel] %s\n' "$*"; }
fail() { printf '[Relay Panel] 错误：%s\n' "$*" >&2; exit 1; }
usage() {
  printf '%s\n' \
    '用法：uninstall.sh [--agent | --control | --all] [--purge] [--yes]' \
    '  默认保留可恢复备份；--purge 会永久删除当前安装和主控数据卷。'
}

for arg in "$@"; do
  case "${arg}" in
    --agent) role="agent" ;;
    --control) role="control" ;;
    --all) role="all" ;;
    --purge) purge=true ;;
    --yes|-y) assume_yes=true ;;
    --help|-h) usage; exit 0 ;;
    *) fail "未知参数：${arg}" ;;
  esac
done

[[ "$(uname -s)" == "Linux" ]] || fail "卸载脚本仅支持 Linux"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
[[ "${install_dir}" == /opt/* && "${install_dir}" != /opt/ ]] || fail "主控安装目录必须是 /opt 下的具体目录"

has_agent=false
has_control=false
[[ -f /etc/relay-agent/config.json || -f /etc/systemd/system/relay-agent.service ]] && has_agent=true
[[ -f "${install_dir}/docker-compose.yml" || -f "${install_dir}/.env" ]] && has_control=true

if [[ -z "${role}" ]]; then
  if [[ "${has_agent}" == true && "${has_control}" == false ]]; then
    role="agent"
  elif [[ "${has_control}" == true && "${has_agent}" == false ]]; then
    role="control"
  elif [[ "${has_control}" == true && "${has_agent}" == true ]]; then
    fail "同时检测到主控和 Agent，请明确添加 --agent、--control 或 --all"
  else
    fail "没有检测到 Relay Panel 安装"
  fi
fi

if [[ "${assume_yes}" != true ]]; then
  action="卸载 ${role} 并保留可恢复备份"
  [[ "${purge}" == true ]] && action="永久清除 ${role}（包括主控数据卷）"
  printf '[Relay Panel] 即将%s。输入 YES 继续：' "${action}" >/dev/tty
  read -r answer </dev/tty || fail "无法读取确认，请在交互式 SSH 中运行，或明确添加 --yes"
  [[ "${answer}" == "YES" ]] || { say "已取消"; exit 0; }
fi

timestamp="$(date +%Y%m%d-%H%M%S)"
backup_root="/var/backups/relay-panel"

uninstall_agent() {
  local backup_file iface
  local -a backup_paths=()
  [[ "${has_agent}" == true ]] || { say "未检测到 Agent，跳过"; return; }
  systemctl disable --now relay-agent.service >/dev/null 2>&1 || true

  if [[ "${purge}" != true ]]; then
    mkdir -p "${backup_root}"
    [[ -d /etc/relay-agent ]] && backup_paths+=(etc/relay-agent)
    [[ -d /var/lib/relay-agent ]] && backup_paths+=(var/lib/relay-agent)
    [[ -f /etc/systemd/system/relay-agent.service ]] && backup_paths+=(etc/systemd/system/relay-agent.service)
    if (( ${#backup_paths[@]} > 0 )); then
      backup_file="${backup_root}/agent-${timestamp}.tar.gz"
      tar -czf "${backup_file}" -C / "${backup_paths[@]}"
      say "Agent 配置与状态已备份到 ${backup_file}"
    fi
  fi

  if command -v nft >/dev/null 2>&1; then
    nft delete table inet relay_panel >/dev/null 2>&1 || true
  fi
  if command -v tc >/dev/null 2>&1 && command -v ip >/dev/null 2>&1; then
    while IFS= read -r iface; do
      [[ -n "${iface}" ]] || continue
      tc qdisc del dev "${iface}" root handle 7a1: >/dev/null 2>&1 || true
      if tc filter show dev "${iface}" ingress 2>/dev/null | grep -q 'ifb-relay0'; then
        tc qdisc del dev "${iface}" ingress >/dev/null 2>&1 || true
      fi
    done < <(ip -o link show | awk -F': ' '{sub(/@.*/,"",$2); print $2}')
    ip link del ifb-relay0 >/dev/null 2>&1 || true
  fi

  rm -f /etc/systemd/system/relay-agent.service /usr/local/bin/relay-agent
  rm -rf /etc/relay-agent /var/lib/relay-agent
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl reset-failed relay-agent.service >/dev/null 2>&1 || true
  [[ -e "${install_dir}" ]] || rm -f /usr/local/bin/zf
  say "Agent 已卸载；不会删除系统 nftables、tc、ping 或 Realm 软件包"
}

uninstall_control() {
  local backup_dir volume_name="" control_id=""
  [[ "${has_control}" == true ]] || { say "未检测到主控，跳过"; return; }
  if command -v docker >/dev/null 2>&1 && [[ -f "${install_dir}/docker-compose.yml" ]]; then
    control_id="$(docker compose --project-directory "${install_dir}" ps -q control 2>/dev/null || true)"
    if [[ -n "${control_id}" ]]; then
      volume_name="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "${control_id}" 2>/dev/null || true)"
    fi
    if [[ "${purge}" == true ]]; then
      docker compose --project-directory "${install_dir}" down --volumes --remove-orphans || true
    else
      docker compose --project-directory "${install_dir}" down --remove-orphans || true
    fi
  fi

  if [[ "${purge}" == true ]]; then
    rm -rf "${install_dir}"
    if [[ -f /var/lib/relay-panel/build.swap ]]; then
      swapoff /var/lib/relay-panel/build.swap >/dev/null 2>&1 || true
      sed -i '\|/var/lib/relay-panel/build.swap none swap sw 0 0|d' /etc/fstab 2>/dev/null || true
      rm -f /var/lib/relay-panel/build.swap
    fi
    rmdir /var/lib/relay-panel >/dev/null 2>&1 || true
    say "主控程序和 Compose 数据卷已永久删除"
  else
    backup_dir="${install_dir}.backup-${timestamp}"
    mv "${install_dir}" "${backup_dir}"
    say "主控文件已移动到 ${backup_dir}"
    if [[ -n "${volume_name}" ]]; then
      say "数据库所在 Docker 卷 ${volume_name} 已保留，可用于恢复"
    else
      say "Docker 数据卷未删除，可用于恢复"
    fi
  fi
  [[ -f /etc/systemd/system/relay-agent.service ]] || rm -f /usr/local/bin/zf
  say "Docker 与 Compose 可能被其他应用共用，因此不会自动卸载"
}

case "${role}" in
  agent) uninstall_agent ;;
  control) uninstall_control ;;
  all) uninstall_agent; uninstall_control ;;
esac

say "卸载完成"
