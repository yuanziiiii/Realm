#!/usr/bin/env bash
set -euo pipefail

install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"
env_file="${install_dir}/.env"

say() { printf '[Relay Panel] %s\n' "$*"; }
fail() { printf '[Relay Panel] 错误：%s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || fail "密码重置仅支持 Linux 控制端"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
command -v docker >/dev/null 2>&1 || fail "没有找到 Docker"
docker compose version >/dev/null 2>&1 || fail "没有找到 Docker Compose"
[[ -f "${env_file}" && -f "${install_dir}/docker-compose.yml" ]] || fail "没有找到 ${install_dir} 中的 Relay Panel"
[[ -r /dev/tty ]] || fail "当前终端无法安全读取密码，请在 SSH 终端直接执行"

printf '请输入新管理员密码（至少 10 位，仅使用字母、数字和 ._@+-）：' > /dev/tty
IFS= read -r -s new_password < /dev/tty
printf '\n请再次输入新密码：' > /dev/tty
IFS= read -r -s confirm_password < /dev/tty
printf '\n' > /dev/tty

[[ "${#new_password}" -ge 10 ]] || fail "密码至少需要 10 位"
[[ "${new_password}" =~ ^[A-Za-z0-9._@+-]+$ ]] || fail "密码只能包含字母、数字和 ._@+-"
[[ "${new_password}" == "${confirm_password}" ]] || fail "两次输入的密码不一致"
unset confirm_password

install_sqlite() {
  if command -v sqlite3 >/dev/null 2>&1; then
    return
  fi
  say "安装轻量级 SQLite 工具"
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y
    DEBIAN_FRONTEND=noninteractive apt-get install -y sqlite3
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y sqlite
  elif command -v yum >/dev/null 2>&1; then
    yum install -y sqlite
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache sqlite
  else
    fail "无法自动安装 sqlite3"
  fi
}

install_sqlite

control_id="$(docker compose --project-directory "${install_dir}" ps -aq control)"
[[ -n "${control_id}" ]] || fail "没有找到 control 容器"
volume_name="$(docker inspect --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "${control_id}")"
[[ -n "${volume_name}" ]] || fail "没有找到控制端数据卷"
volume_path="$(docker volume inspect --format '{{.Mountpoint}}' "${volume_name}")"
db_path="${volume_path}/relay-panel.db"
[[ -f "${db_path}" ]] || fail "没有找到控制端数据库"

tmp_env="$(mktemp)"
restart_needed=false
cleanup() {
  unset new_password
  rm -f "${tmp_env}"
  if [[ "${restart_needed}" == true ]]; then
    docker compose --project-directory "${install_dir}" up -d control >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

say "停止控制端并备份数据库"
docker compose --project-directory "${install_dir}" stop control >/dev/null
restart_needed=true
backup_path="${db_path}.before-password-reset.$(date +%Y%m%d%H%M%S)"
cp -a "${db_path}" "${backup_path}"

awk -v password="${new_password}" '
  BEGIN { found = 0 }
  /^RELAY_ADMIN_PASSWORD=/ { print "RELAY_ADMIN_PASSWORD=" password; found = 1; next }
  { print }
  END { if (!found) print "RELAY_ADMIN_PASSWORD=" password }
' "${env_file}" > "${tmp_env}"
install -m 0600 "${tmp_env}" "${env_file}"

sqlite3 "${db_path}" "DELETE FROM settings WHERE key='admin_password_hash';"
say "启动控制端并写入新密码"
docker compose --project-directory "${install_dir}" up -d --force-recreate control >/dev/null
sleep 3

control_id="$(docker compose --project-directory "${install_dir}" ps -q control)"
[[ -n "${control_id}" && "$(docker inspect --format '{{.State.Status}}' "${control_id}")" == "running" ]] || fail "控制端没有正常启动，数据库备份位于 ${backup_path}"
restart_needed=false

unset new_password
say "管理员密码已重置，可以使用新密码登录"
say "数据库备份：${backup_path}"
