#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
version="${RELAY_PANEL_VERSION:-latest}"
install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"

say() { printf '[Relay Panel] %s\n' "$*"; }
fail() { printf '[Relay Panel] 错误：%s\n' "$*" >&2; exit 1; }

[[ "$(uname -s)" == "Linux" ]] || fail "控制端更新仅支持 Linux"
[[ "${EUID}" -eq 0 ]] || fail "请使用 root 运行（curl ... | sudo bash）"
command -v docker >/dev/null 2>&1 || fail "没有找到 Docker"
command -v curl >/dev/null 2>&1 || fail "没有找到 curl"
command -v sha256sum >/dev/null 2>&1 || fail "没有找到 sha256sum"
docker compose version >/dev/null 2>&1 || fail "没有找到 Docker Compose"
[[ -f "${install_dir}/docker-compose.yml" ]] || fail "没有找到 ${install_dir} 中的 Relay Panel"

case "$(uname -m)" in
  x86_64|amd64) control_arch="amd64" ;;
  aarch64|arm64) control_arch="arm64" ;;
  *) fail "暂不支持的 CPU 架构：$(uname -m)" ;;
esac

if [[ "${version}" == "latest" ]]; then
  download_base="https://github.com/${repo}/releases/latest/download"
else
  download_base="https://github.com/${repo}/releases/download/${version}"
fi

ensure_build_swap() {
  local memory_kb swap_kb swap_dir swap_file
  memory_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
  swap_kb="$(awk '/^SwapTotal:/ {print $2}' /proc/meminfo)"
  if (( memory_kb >= 1572864 || swap_kb >= 1572864 )); then
    return
  fi
  swap_dir="/var/lib/relay-panel"
  swap_file="${swap_dir}/build.swap"
  say "检测到内存不足 1.5 GB，准备 2 GB 构建交换空间"
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

control_id="$(docker compose --project-directory "${install_dir}" ps -aq control)"
web_id="$(docker compose --project-directory "${install_dir}" ps -aq web)"
[[ -n "${control_id}" && -n "${web_id}" ]] || fail "没有找到 control 或 web 容器"
control_image_ref="$(docker inspect --format '{{.Config.Image}}' "${control_id}")"
web_image_ref="$(docker inspect --format '{{.Config.Image}}' "${web_id}")"
old_control_image_id="$(docker image inspect --format '{{.Id}}' "${control_image_ref}")"
old_web_image_id="$(docker image inspect --format '{{.Id}}' "${web_image_ref}")"
[[ -n "${old_control_image_id}" && -n "${old_web_image_id}" ]] || fail "无法识别当前镜像版本"

tmp_dir="$(mktemp -d)"
candidate_suffix="$(date +%s)-$$"
candidate_control="relay-panel-control-update:${candidate_suffix}"
candidate_web="relay-panel-web-update:${candidate_suffix}"
cleanup() {
  docker image rm "${candidate_control}" "${candidate_web}" >/dev/null 2>&1 || true
  rm -rf "${tmp_dir}"
}
trap cleanup EXIT

download_checked() {
  local name="$1" target="$2" expected actual
  curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${name}" -o "${target}"
  curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${name}.sha256" -o "${target}.sha256"
  expected="$(awk '{print $1}' "${target}.sha256")"
  actual="$(sha256sum "${target}" | awk '{print $1}')"
  [[ -n "${expected}" && "${actual}" == "${expected}" ]] || fail "${name} 的 SHA-256 校验失败"
}

binary_name="relay-control-linux-${control_arch}"
say "下载并校验控制端、网页端完整更新包"
download_checked "${binary_name}" "${tmp_dir}/relay-control"
download_checked "relay-panel-source.tar.gz" "${tmp_dir}/source.tar.gz"
chmod 0755 "${tmp_dir}/relay-control"
mkdir -p "${tmp_dir}/source"
tar -xzf "${tmp_dir}/source.tar.gz" -C "${tmp_dir}/source"

cat > "${tmp_dir}/Dockerfile.control" <<'EOF'
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S relay \
    && adduser -S -G relay relay \
    && mkdir -p /data \
    && chown relay:relay /data
COPY relay-control /usr/local/bin/relay-control
USER relay
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/relay-control"]
EOF

ensure_build_swap
say "构建网页端（1C1G 会使用交换空间，通常需要约 1 分钟）"
COMPOSE_PARALLEL_LIMIT=1 docker build --tag "${candidate_web}" --file "${tmp_dir}/source/Dockerfile.web" "${tmp_dir}/source"
say "构建轻量控制端"
docker build --quiet --tag "${candidate_control}" --file "${tmp_dir}/Dockerfile.control" "${tmp_dir}" >/dev/null

docker tag "${candidate_web}" "${web_image_ref}"
docker tag "${candidate_control}" "${control_image_ref}"
say "重建 web 与 control 容器"
if ! docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate web control >/dev/null; then
  docker tag "${old_web_image_id}" "${web_image_ref}"
  docker tag "${old_control_image_id}" "${control_image_ref}"
  docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate web control >/dev/null 2>&1 || true
  fail "容器重建失败，已恢复旧版网页端和控制端"
fi
healthy=false
new_control_id=""
new_web_id=""
for _ in {1..10}; do
  new_control_id="$(docker compose --project-directory "${install_dir}" ps -q control)"
  new_web_id="$(docker compose --project-directory "${install_dir}" ps -q web)"
  if [[ -n "${new_control_id}" && -n "${new_web_id}" ]] \
    && [[ "$(docker inspect --format '{{.State.Status}}' "${new_control_id}")" == "running" ]] \
    && [[ "$(docker inspect --format '{{.State.Status}}' "${new_web_id}")" == "running" ]] \
    && docker exec "${new_control_id}" wget -q -O /dev/null http://127.0.0.1:8080/healthz \
    && docker exec "${new_control_id}" wget -q -O /dev/null http://web:3000/; then
    healthy=true
    break
  fi
  sleep 3
done

if [[ "${healthy}" == "true" ]]; then
  # Keep deployment files in sync for the next update while preserving .env
  # and the named database volume.
  tar -C "${tmp_dir}/source" -cf - . | tar -C "${install_dir}" -xf -
  say "完整更新完成：网页端、控制端、数据库兼容层均已更新"
  say "管理员密码、HTTPS 反代配置和数据库均未改动"
  exit 0
fi

say "新版健康检查失败，正在恢复旧版网页端和控制端"
[[ -n "${new_control_id}" ]] && docker logs --tail 30 "${new_control_id}" >&2 || true
[[ -n "${new_web_id}" ]] && docker logs --tail 30 "${new_web_id}" >&2 || true
docker tag "${old_web_image_id}" "${web_image_ref}"
docker tag "${old_control_image_id}" "${control_image_ref}"
docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate web control >/dev/null 2>&1 || true
fail "控制端更新失败，已恢复旧版本"
