#!/usr/bin/env bash
set -uo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"
agent_config="/etc/relay-agent/config.json"
agent_service="relay-agent.service"

if [[ "$(uname -s)" != "Linux" ]]; then
  printf 'zf 仅支持 Linux。\n' >&2
  exit 1
fi
if [[ "${EUID}" -ne 0 ]]; then
  if command -v sudo >/dev/null 2>&1; then
    exec sudo -- "$0" "$@"
  fi
  printf '请使用 root 运行 zf。\n' >&2
  exit 1
fi

if [[ -t 1 && "${TERM:-dumb}" != "dumb" ]]; then
  green='\033[1;32m'; cyan='\033[1;36m'; yellow='\033[1;33m'; red='\033[1;31m'; reset='\033[0m'
else
  green=''; cyan=''; yellow=''; red=''; reset=''
fi

say() { printf '%b[Relay Panel]%b %s\n' "${cyan}" "${reset}" "$*"; }
warn() { printf '%b提示：%b%s\n' "${yellow}" "${reset}" "$*"; }
pause_menu() {
  [[ -t 0 ]] || return 0
  printf '\n按 Enter 返回菜单...' > /dev/tty
  IFS= read -r _ < /dev/tty || true
}
clear_screen() {
  if command -v clear >/dev/null 2>&1 && [[ -t 1 ]]; then clear; fi
}
confirm() {
  local prompt="$1" answer
  printf '%s [y/N]：' "${prompt}" > /dev/tty
  IFS= read -r answer < /dev/tty || return 1
  [[ "${answer}" == "y" || "${answer}" == "Y" ]]
}
run_release_script() {
  local name="$1" tmp_file checksum_file expected actual controller_url local_base downloaded
  shift
  command -v curl >/dev/null 2>&1 || { warn '缺少 curl，无法在线执行'; return 1; }
  command -v sha256sum >/dev/null 2>&1 || { warn '缺少 sha256sum，无法校验更新文件'; return 1; }
  tmp_file="$(mktemp)" || return 1
  checksum_file="${tmp_file}.sha256"
  downloaded=false
  controller_url=""
  if [[ -s "${agent_config}" ]] && command -v jq >/dev/null 2>&1; then
    controller_url="$(jq -r '.controller_url // empty' "${agent_config}" 2>/dev/null || true)"
  fi
  local_base="${controller_url%/}/downloads"
  if [[ -n "${controller_url}" ]] \
    && curl --proto '=http,https' --proto-redir '=http,https' --tlsv1.2 -fsSL "${local_base}/${name}" -o "${tmp_file}" \
    && curl --proto '=http,https' --proto-redir '=http,https' --tlsv1.2 -fsSL "${local_base}/${name}.sha256" -o "${checksum_file}"; then
    downloaded=true
    say "已从控制端本地下载源获取 ${name}"
  fi
  if [[ "${downloaded}" != true ]] \
    && { ! curl --proto '=https' --tlsv1.2 -fsSL "https://github.com/${repo}/releases/latest/download/${name}" -o "${tmp_file}" \
    || ! curl --proto '=https' --tlsv1.2 -fsSL "https://github.com/${repo}/releases/latest/download/${name}.sha256" -o "${checksum_file}"; }; then
    rm -f "${tmp_file}" "${checksum_file}"
    warn "下载 ${name} 失败"
    return 1
  fi
  expected="$(awk '{print $1}' "${checksum_file}")"
  actual="$(sha256sum "${tmp_file}" | awk '{print $1}')"
  if [[ -z "${expected}" || "${actual}" != "${expected}" ]]; then
    rm -f "${tmp_file}" "${checksum_file}"
    warn "${name} 的 SHA-256 校验失败，已停止执行"
    return 1
  fi
  bash "${tmp_file}" "$@"
  local result=$?
  rm -f "${tmp_file}" "${checksum_file}"
  return "${result}"
}

has_control() {
  [[ -f "${install_dir}/docker-compose.yml" && -f "${install_dir}/.env" ]] \
    && command -v docker >/dev/null 2>&1
}
has_agent() {
  [[ -s "${agent_config}" ]] \
    && command -v systemctl >/dev/null 2>&1 \
    && systemctl cat "${agent_service}" >/dev/null 2>&1
}
compose() { docker compose --project-directory "${install_dir}" "$@"; }

control_status() {
  local current_version
  current_version="$(tr -d '[:space:]' < "${install_dir}/.relay-panel-version" 2>/dev/null || true)"
  say "主控安装目录：${install_dir}"
  say "当前版本：${current_version:-未记录（首次更新后自动记录）}"
  compose ps || true
}
control_update() {
  say '检测本机版本与 GitHub 最新稳定版'
  run_release_script update.sh
}
control_restart() { say '重启网页端与控制端'; compose restart web control; }
control_start() { say '启动网页端与控制端'; compose up -d --no-build; }
control_stop() {
  confirm '停止后面板和 Agent 同步会暂时中断，确认停止？' || return 0
  compose stop web control
}
control_logs() {
  local choice
  printf '1. 最近 120 行\n2. 实时跟踪（Ctrl+C 返回）\n请选择：' > /dev/tty
  IFS= read -r choice < /dev/tty || return 0
  case "${choice}" in
    1) compose logs --tail 120 control web || true ;;
    2) compose logs --tail 80 -f control web || true ;;
    *) warn '已取消' ;;
  esac
}
control_health() {
  local http_port control_id web_id
  http_port="$(awk -F= '$1=="RELAY_HTTP_PORT" {print $2}' "${install_dir}/.env" 2>/dev/null | tail -n 1)"
  http_port="${http_port:-8080}"
  say '检查容器状态'
  control_id="$(compose ps -q control 2>/dev/null || true)"
  web_id="$(compose ps -q web 2>/dev/null || true)"
  [[ -n "${control_id}" && "$(docker inspect --format '{{.State.Status}}' "${control_id}" 2>/dev/null)" == 'running' ]] \
    && printf '%b✓%b control 正常运行\n' "${green}" "${reset}" \
    || printf '%b✗%b control 未运行\n' "${red}" "${reset}"
  [[ -n "${web_id}" && "$(docker inspect --format '{{.State.Status}}' "${web_id}" 2>/dev/null)" == 'running' ]] \
    && printf '%b✓%b web 正常运行\n' "${green}" "${reset}" \
    || printf '%b✗%b web 未运行\n' "${red}" "${reset}"
  say "检查本机接口 http://127.0.0.1:${http_port}/healthz"
  if curl -fsS --connect-timeout 5 "http://127.0.0.1:${http_port}/healthz"; then
    printf '\n%b✓%b 主控健康接口正常\n' "${green}" "${reset}"
  else
    printf '%b✗%b 主控健康接口异常，请查看日志\n' "${red}" "${reset}"
  fi
}
control_password() {
  warn '这里用于忘记密码时重置；能正常登录时请优先在“系统设置”修改密码。'
  confirm '继续从 SSH 重置管理员密码？' || return 0
  run_release_script reset-admin-password.sh
}
control_info() {
  local http_port host_ip secure current_version
  http_port="$(awk -F= '$1=="RELAY_HTTP_PORT" {print $2}' "${install_dir}/.env" 2>/dev/null | tail -n 1)"
  secure="$(awk -F= '$1=="RELAY_SECURE_COOKIES" {print $2}' "${install_dir}/.env" 2>/dev/null | tail -n 1)"
  host_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
  current_version="$(tr -d '[:space:]' < "${install_dir}/.relay-panel-version" 2>/dev/null || true)"
  printf '当前版本：%s\n安装目录：%s\n本机访问：http://%s:%s\n安全 Cookie：%s\n数据位置：Docker volume（relay-panel.db）\n' \
    "${current_version:-未记录}" "${install_dir}" "${host_ip:-服务器IP}" "${http_port:-8080}" "${secure:-false}"
  warn 'HTTPS 域名由你的 1Panel、宝塔或其他反向代理管理，zf 不会覆盖反代配置。'
}
control_uninstall() {
  warn '卸载主控会停止面板；卸载脚本将要求输入大写 YES，并默认保留数据库卷与可恢复目录。'
  run_release_script uninstall.sh --control && exit 0
}

agent_status() { systemctl status "${agent_service}" --no-pager -l || true; }
agent_update() {
  confirm '确认在线更新 Agent？Node ID、Token 和主控地址会保留' || return 0
  run_release_script update-agent.sh
}
agent_restart() { systemctl restart "${agent_service}" && systemctl --no-pager --full status "${agent_service}"; }
agent_start() { systemctl start "${agent_service}" && systemctl --no-pager --full status "${agent_service}"; }
agent_stop() {
  confirm '停止后本机现有转发仍可能继续，但不会再接收面板变更，确认停止？' || return 0
  systemctl stop "${agent_service}"
}
agent_logs() {
  local choice
  printf '1. 最近 120 行\n2. 实时跟踪（Ctrl+C 返回）\n请选择：' > /dev/tty
  IFS= read -r choice < /dev/tty || return 0
  case "${choice}" in
    1) journalctl -u "${agent_service}" -n 120 --no-pager -l || true ;;
    2) journalctl -u "${agent_service}" -n 60 -f -l || true ;;
    *) warn '已取消' ;;
  esac
}
agent_connectivity() {
  local controller_url
  controller_url="$(read_agent_value controller_url)"
  [[ -n "${controller_url}" ]] || { warn '配置中没有主控地址'; return 1; }
  say "检查 ${controller_url%/}/healthz"
  if curl -fsS --connect-timeout 8 --max-time 20 "${controller_url%/}/healthz"; then
    printf '\n%b✓%b DNS、端口、HTTPS 和反向代理可达\n' "${green}" "${reset}"
  else
    printf '%b✗%b 无法访问主控，请检查 DNS、443/面板端口、防火墙和反向代理\n' "${red}" "${reset}"
  fi
}
read_agent_value() {
  local key="$1"
  if command -v jq >/dev/null 2>&1; then
    jq -r --arg key "${key}" '.[$key] // empty' "${agent_config}" 2>/dev/null
  else
    sed -n 's/.*"'"${key}"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "${agent_config}" | head -n 1
  fi
}
agent_info() {
  local token_state target_probe_interval
  [[ -n "$(read_agent_value token)" ]] && token_state='已配置（不会显示明文）' || token_state='缺失'
  target_probe_interval="$(read_agent_value target_probe_interval)"
  [[ -n "${target_probe_interval}" ]] || target_probe_interval='60s（默认）'
  printf 'Node ID：%s\n主控地址：%s\n同步周期：%s\n落地探测周期：%s\nAgent Token：%s\n配置文件：%s\n' \
    "$(read_agent_value node_id)" "$(read_agent_value controller_url)" "$(read_agent_value sync_interval)" "${target_probe_interval}" "${token_state}" "${agent_config}"
}
agent_environment() {
  local failed=0
  for command_name in nft tc ip; do
    if command -v "${command_name}" >/dev/null 2>&1; then
      printf '%b✓%b %-10s %s\n' "${green}" "${reset}" "${command_name}" "$(command -v "${command_name}")"
    else
      printf '%b✗%b %-10s 未安装\n' "${red}" "${reset}" "${command_name}"
      failed=1
    fi
  done
  if command -v realm >/dev/null 2>&1; then
    printf '%b✓%b Realm      %s\n' "${green}" "${reset}" "$(command -v realm)"
  else
    printf '%b-%b Realm      未安装（只使用 nftables 时正常）\n' "${yellow}" "${reset}"
  fi
  if [[ -r /proc/net/nf_conntrack || -r /proc/net/ip_conntrack ]] || command -v conntrack >/dev/null 2>&1; then
    printf '%b✓%b 连接采集   可读取 conntrack\n' "${green}" "${reset}"
  else
    printf '%b-%b 连接采集   当前不可用（不影响转发）\n' "${yellow}" "${reset}"
  fi
  [[ "$(sysctl -n net.ipv4.ip_forward 2>/dev/null || true)" == '1' ]] \
    && printf '%b✓%b IPv4 转发 已开启\n' "${green}" "${reset}" \
    || { printf '%b✗%b IPv4 转发 未开启\n' "${red}" "${reset}"; failed=1; }
  systemctl is-active --quiet "${agent_service}" \
    && printf '%b✓%b Agent 服务 正常运行\n' "${green}" "${reset}" \
    || { printf '%b✗%b Agent 服务 未运行\n' "${red}" "${reset}"; failed=1; }
  return "${failed}"
}
agent_access_rule_name() {
  local suffix="$1" counter_name="rp_${1}_up" counter_json name
  if command -v jq >/dev/null 2>&1 \
    && counter_json="$(nft -j list counter inet relay_panel "${counter_name}" 2>/dev/null)"; then
    name="$(printf '%s' "${counter_json}" | jq -r --arg counter "${counter_name}" \
      '.nftables[] | select(.counter.name? == $counter) | .counter.comment // empty' 2>/dev/null | head -n 1)"
  fi
  if [[ -z "${name:-}" ]]; then
    name="$(nft list counter inet relay_panel "${counter_name}" 2>/dev/null \
      | sed -n 's/^[[:space:]]*comment "\(.*\)"[[:space:]]*$/\1/p' | head -n 1)"
  fi
  name="${name% upload}"
  [[ -n "${name}" ]] && printf '%s' "${name}" || printf '规则 %s' "${suffix}"
}
agent_access_select() {
  local chain line port proto set_name raw_set suffix kind key choice index name protocol_text
  local -a keys=()
  local -A protocols=()
  if ! chain="$(nft -a list chain inet relay_panel access_control 2>/dev/null)"; then
    warn '没有找到 relay_panel/access_control；请确认 Agent 正常运行并已收到规则'
    return 1
  fi
  while IFS= read -r line; do
    [[ "${line}" =~ (tcp|udp)[[:space:]]+dport[[:space:]]+([0-9]+) ]] || continue
    proto="${BASH_REMATCH[1]}"; port="${BASH_REMATCH[2]}"
    set_name="$(printf '%s\n' "${line}" | grep -oE '@ac_[[:alnum:]_]+' | head -n 1 || true)"
    [[ -n "${set_name}" ]] || continue
    raw_set="${set_name#@ac_}"; suffix=''
    for kind in manual_allow geo_allow tcp_conn tcp_rate udp_rate deny; do
      if [[ "${raw_set}" == *"_${kind}" ]]; then
        suffix="${raw_set%_${kind}}"
        break
      fi
    done
    [[ -n "${suffix}" ]] || continue
    key="${suffix}|${port}"
    if [[ -z "${protocols[${key}]+x}" ]]; then
      keys+=("${key}")
      protocols["${key}"]="${proto}"
    elif [[ ",${protocols[${key}]}," != *",${proto},"* ]]; then
      protocols["${key}"]+=",${proto}"
    fi
  done <<< "${chain}"
  if (( ${#keys[@]} == 0 )); then
    printf '%b-%b 当前机器没有启用访问控制或连接限制的入口规则\n' "${yellow}" "${reset}"
    return 0
  fi
  printf '%b可检查的入口规则%b\n\n' "${cyan}" "${reset}"
  index=1
  for key in "${keys[@]}"; do
    suffix="${key%%|*}"; port="${key##*|}"
    name="$(agent_access_rule_name "${suffix}")"
    case ",${protocols[${key}]}," in
      *,tcp,*udp,*|*,udp,*tcp,*) protocol_text='TCP+UDP' ;;
      *,tcp,*) protocol_text='TCP' ;;
      *) protocol_text='UDP' ;;
    esac
    printf ' %b%d.%b %-28s 端口 %-5s %s\n' "${green}" "${index}" "${reset}" "${name}" "${port}" "${protocol_text}"
    index=$((index + 1))
  done
  printf ' %b0.%b 返回\n\n请选择规则：' "${cyan}" "${reset}" > /dev/tty
  IFS= read -r choice < /dev/tty || return 0
  [[ "${choice}" == '0' ]] && return 0
  if [[ ! "${choice}" =~ ^[0-9]+$ ]] || (( choice < 1 || choice > ${#keys[@]} )); then
    warn "请输入 0-${#keys[@]}"
    return 2
  fi
  key="${keys[$((choice - 1))]}"
  agent_access_check "${key##*|}" "${key%%|*}"
}
agent_access_check() {
  local port="${1:-}" rule_suffix="${2:-}" chain matches set_names set_name set_json count sample label rule_name
  if [[ -z "${port}" && -t 0 ]]; then
    printf '请输入要检查的入口端口：' > /dev/tty
    IFS= read -r port < /dev/tty || return 0
  fi
  if [[ ! "${port}" =~ ^[0-9]+$ ]] || (( port < 1 || port > 65535 )); then
    warn '端口必须是 1-65535 之间的数字，例如：zf access 31259'
    return 2
  fi
  command -v nft >/dev/null 2>&1 || { warn '本机没有安装 nft，无法检查访问控制'; return 1; }
  if ! chain="$(nft -a list chain inet relay_panel access_control 2>/dev/null)"; then
    warn '没有找到 relay_panel/access_control；请确认 Agent 正常运行并已收到规则'
    return 1
  fi
  matches="$(printf '%s\n' "${chain}" | awk -v port="${port}" '$0 ~ ("dport " port "([^0-9]|$)")')"
  if [[ -n "${rule_suffix}" ]]; then
    matches="$(printf '%s\n' "${matches}" | grep -F "@ac_${rule_suffix}_" || true)"
    rule_name="$(agent_access_rule_name "${rule_suffix}")"
    printf '%b%s · 端口 %s · 访问控制检查%b\n' "${cyan}" "${rule_name}" "${port}" "${reset}"
  else
    printf '%b端口 %s · 访问控制检查%b\n' "${cyan}" "${port}" "${reset}"
  fi
  if [[ -r /var/lib/relay-agent/state.json ]] && command -v jq >/dev/null 2>&1; then
    printf 'Agent 修订：%s · 应用状态：%s\n' \
      "$(jq -r '.applied_revision // 0' /var/lib/relay-agent/state.json 2>/dev/null)" \
      "$(jq -r '.apply_status // "未知"' /var/lib/relay-agent/state.json 2>/dev/null)"
  fi
  if [[ -z "${matches}" ]]; then
    printf '%b-%b 当前端口没有 IP、地区或连接限制规则\n' "${yellow}" "${reset}"
    warn '如果面板已经启用策略，请等待约 10 秒后重试，并检查 Agent 日志。'
    return 0
  fi
  printf '\n判定顺序（由上到下，命中 accept/drop 后停止）：\n'
  printf '%s\n' "${matches}" | sed 's/^[[:space:]]*/  /'
  set_names="$(printf '%s\n' "${matches}" | grep -oE '@[[:alnum:]_]+' | tr -d '@' | sort -u || true)"
  printf '\n内核集合：\n'
  while IFS= read -r set_name; do
    [[ -n "${set_name}" ]] || continue
    case "${set_name}" in
      *_manual_allow) label='IP/CIDR 优先放行' ;;
      *_deny) label='IP/地区禁止' ;;
      *_geo_allow) label='允许省市网段' ;;
      *_tcp_conn) label='TCP 并发限制状态' ;;
      *_tcp_rate) label='TCP 新建频率状态' ;;
      *_udp_rate) label='UDP 新建频率状态' ;;
      *) label='访问控制集合' ;;
    esac
    if command -v jq >/dev/null 2>&1 && set_json="$(nft -j list set inet relay_panel "${set_name}" 2>/dev/null)"; then
      count="$(printf '%s' "${set_json}" | jq -r '[.nftables[] | select(.set != null) | (.set.elem // [])[]] | length' 2>/dev/null || printf '?')"
      sample="$(printf '%s' "${set_json}" | jq -c '[.nftables[] | select(.set != null) | (.set.elem // [])[]][0:8]' 2>/dev/null || true)"
      printf '  %b✓%b %s：%s 个元素' "${green}" "${reset}" "${label}" "${count}"
      [[ -n "${sample}" && "${sample}" != '[]' ]] && printf ' · 示例 %s' "${sample}"
      printf '\n'
    elif nft list set inet relay_panel "${set_name}" >/dev/null 2>&1; then
      printf '  %b✓%b %s：已安装\n' "${green}" "${reset}" "${label}"
    else
      printf '  %b✗%b %s：规则引用但集合不存在\n' "${red}" "${reset}" "${label}"
    fi
  done <<< "${set_names}"
  if printf '%s' "${matches}" | grep -q '_deny' \
    && printf '%s' "${matches}" | grep -q '_geo_allow' \
    && nft list set inet relay_panel "$(printf '%s\n' "${set_names}" | grep '_deny$' | head -n 1)" 2>/dev/null | grep -q '0\.0\.0\.0/0'; then
    warn '检测到黑名单 0.0.0.0/0 与城市白名单同时启用：/0 会先拦截，城市白名单不会获得放行机会。'
  fi
  printf '\n说明：drop 行的 packets/bytes 增长，表示拦截正在生效；tcpdump 仍能看到到达网卡但已被丢弃的数据包。\n'
}
agent_uninstall() {
  warn '卸载 Agent 会停止同步并清理本面板创建的转发；卸载脚本将要求输入大写 YES。'
  run_release_script uninstall.sh --agent && exit 0
}

print_header() {
  local role="$1"
  printf '%b========================================%b\n' "${cyan}" "${reset}"
  printf '%b Relay Panel · zf SSH 管理工具%b\n' "${green}" "${reset}"
  printf ' 当前角色：%s\n' "${role}"
  printf '%b========================================%b\n' "${cyan}" "${reset}"
}
control_menu() {
  local choice
  while true; do
    clear_screen; print_header '主控端'
    printf ' %b1.%b 查看主控状态\n %b2.%b 检测并更新主控\n %b3.%b 重启主控\n %b4.%b 启动主控\n %b5.%b 停止主控\n %b6.%b 查看主控日志\n %b7.%b 运行健康检查\n %b8.%b 重置管理员密码\n %b9.%b 显示访问配置\n %b10.%b 安全卸载主控\n %b0.%b 退出\n\n请选择：' \
      "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" \
      "${green}" "${reset}" "${green}" "${reset}" "${yellow}" "${reset}" "${green}" "${reset}" "${red}" "${reset}" "${cyan}" "${reset}" > /dev/tty
    IFS= read -r choice < /dev/tty || break
    printf '\n'
    case "${choice}" in
      1) control_status ;; 2) control_update ;; 3) control_restart ;; 4) control_start ;; 5) control_stop ;;
      6) control_logs ;; 7) control_health ;; 8) control_password ;; 9) control_info ;; 10) control_uninstall ;; 0) break ;;
      *) warn '请输入 0-10' ;;
    esac
    [[ "${choice}" == '0' ]] || pause_menu
  done
}
agent_menu() {
  local choice
  while true; do
    clear_screen; print_header 'Agent 被控端'
    printf ' %b1.%b 查看 Agent 状态\n %b2.%b 在线更新 Agent\n %b3.%b 重启 Agent\n %b4.%b 启动 Agent\n %b5.%b 停止 Agent\n %b6.%b 查看 Agent 日志\n %b7.%b 测试主控连通性\n %b8.%b 显示节点信息\n %b9.%b 检查转发环境\n %b10.%b 检查规则访问控制\n %b11.%b 安全卸载 Agent\n %b0.%b 退出\n\n请选择：' \
      "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" \
      "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${green}" "${reset}" "${red}" "${reset}" "${cyan}" "${reset}" > /dev/tty
    IFS= read -r choice < /dev/tty || break
    printf '\n'
    case "${choice}" in
      1) agent_status ;; 2) agent_update ;; 3) agent_restart ;; 4) agent_start ;; 5) agent_stop ;;
      6) agent_logs ;; 7) agent_connectivity ;; 8) agent_info ;; 9) agent_environment ;; 10) agent_access_select ;; 11) agent_uninstall ;; 0) break ;;
      *) warn '请输入 0-11' ;;
    esac
    [[ "${choice}" == '0' ]] || pause_menu
  done
}

run_command() {
  local role="$1" command_name="$2" command_arg="${3:-}"
  case "${role}:${command_name}" in
    control:status) control_status ;; control:update) control_update ;; control:restart) control_restart ;;
    control:start) control_start ;; control:stop) control_stop ;; control:logs) control_logs ;;
    control:doctor) control_health ;; control:password) control_password ;; control:info) control_info ;;
    control:uninstall) control_uninstall ;;
    agent:status) agent_status ;; agent:update) agent_update ;; agent:restart) agent_restart ;;
    agent:start) agent_start ;; agent:stop) agent_stop ;; agent:logs) agent_logs ;;
    agent:doctor) agent_connectivity; agent_environment ;; agent:info) agent_info ;;
    agent:access) agent_access_check "${command_arg}" ;;
    agent:uninstall) agent_uninstall ;;
    *) warn "未知命令：${command_name}"; return 2 ;;
  esac
}

control_installed=false; agent_installed=false
has_control && control_installed=true
has_agent && agent_installed=true

if [[ "${control_installed}" != true && "${agent_installed}" != true ]]; then
  warn '当前机器没有检测到 Relay Panel 主控或 Agent。请先完成安装。'
  exit 1
fi

if [[ $# -ge 1 ]]; then
  selected_role="${2:-}"
  command_arg="${3:-}"
  if [[ "${selected_role}" != 'control' && "${selected_role}" != 'agent' ]]; then
    [[ "$1" == 'access' ]] && command_arg="${2:-}"
    if [[ "${agent_installed}" == true ]]; then selected_role='agent'; else selected_role='control'; fi
  fi
  run_command "${selected_role}" "$1" "${command_arg}"
  exit $?
fi

if [[ "${agent_installed}" == true ]]; then
  agent_menu
else
  control_menu
fi
