#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
branch="${RELAY_PANEL_BRANCH:-main}"
install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"
http_port="${RELAY_HTTP_PORT:-8080}"

say() { printf '[Relay Panel] %s\n' "$*"; }
fail() { printf '[Relay Panel] 错误：%s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || fail "控制端一键安装仅支持 Linux"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
[[ "${install_dir}" == /opt/* ]] || fail "安装目录必须位于 /opt 下"
[[ ! -e "${install_dir}" ]] || fail "${install_dir} 已存在；为保护现有数据，本脚本不会覆盖安装"

install_prerequisites() {
  if command -v apt-get >/dev/null 2>&1; then
    apt-get update -y
    DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl openssl tar
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y ca-certificates curl openssl tar
  elif command -v yum >/dev/null 2>&1; then
    yum install -y ca-certificates curl openssl tar
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache ca-certificates curl openssl tar
  else
    fail "不支持当前包管理器，请先手工安装 Docker、curl、openssl 和 tar"
  fi
}

install_docker() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    return
  fi
  say "安装 Docker Engine 与 Compose 插件"
  local docker_script
  docker_script="$(mktemp)"
  curl --proto '=https' --tlsv1.2 -fsSL https://get.docker.com -o "${docker_script}"
  sh "${docker_script}"
  rm -f "${docker_script}"
  systemctl enable --now docker 2>/dev/null || true
  docker compose version >/dev/null 2>&1 || fail "Docker Compose 插件安装失败"
}

ensure_build_swap() {
  local memory_kb swap_kb swap_dir swap_file
  memory_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
  swap_kb="$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo)"
  if (( memory_kb >= 1572864 || swap_kb >= 1572864 )); then
    return
  fi
  swap_dir="/var/lib/relay-panel"
  swap_file="${swap_dir}/build.swap"
  say "检测到内存不足 1.5 GB，创建 2 GB 构建交换空间"
  mkdir -p "${swap_dir}"
  if [[ ! -f "${swap_file}" ]]; then
    if command -v fallocate >/dev/null 2>&1; then
      fallocate -l 2G "${swap_file}"
    else
      dd if=/dev/zero of="${swap_file}" bs=1M count=2048 status=none
    fi
    chmod 600 "${swap_file}"
    mkswap "${swap_file}" >/dev/null
  fi
  swapon "${swap_file}" 2>/dev/null || true
  grep -Fq "${swap_file} none swap sw 0 0" /etc/fstab || printf '%s\n' "${swap_file} none swap sw 0 0" >> /etc/fstab
}

install_prerequisites
install_docker
ensure_build_swap

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
archive_url="https://github.com/${repo}/archive/refs/heads/${branch}.tar.gz"
say "下载 ${repo}@${branch}"
say "下载 Relay Panel 源码包"
curl --proto '=https' --tlsv1.2 -fL --show-error --progress-bar "${archive_url}" -o "${tmp_dir}/source.tar.gz"
mkdir -p "${install_dir}"
tar -xzf "${tmp_dir}/source.tar.gz" --strip-components=1 -C "${install_dir}"

admin_password="${RELAY_ADMIN_PASSWORD:-$(openssl rand -base64 24 | tr -d '\n')}"
session_secret="${RELAY_SESSION_SECRET:-$(openssl rand -hex 32)}"
umask 077
printf '%s\n' \
  "RELAY_ADMIN_PASSWORD=${admin_password}" \
  "RELAY_SESSION_SECRET=${session_secret}" \
  "RELAY_HTTP_PORT=${http_port}" \
  "RELAY_SECURE_COOKIES=false" > "${install_dir}/.env"

say "低内存模式：串行构建控制端"
COMPOSE_PARALLEL_LIMIT=1 docker compose --project-directory "${install_dir}" build control
say "低内存模式：串行构建网页端"
COMPOSE_PARALLEL_LIMIT=1 docker compose --project-directory "${install_dir}" build web
say "启动控制端"
docker compose --project-directory "${install_dir}" up -d --no-build
install -m 0755 "${install_dir}/scripts/zf.sh" /usr/local/bin/zf

host_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"
host_ip="${host_ip:-服务器IP}"
say "安装完成"
printf '\n面板地址：http://%s:%s\n管理员密码：%s\n安装目录：%s\n\n' "${host_ip}" "${http_port}" "${admin_password}" "${install_dir}"
printf '请立即保存密码。公网使用时，请在面板前配置 HTTPS 反向代理。\n'
printf '以后在 SSH 中输入 zf，即可进入主控维护菜单。\n'
