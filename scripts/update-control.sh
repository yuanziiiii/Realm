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

control_id="$(docker compose --project-directory "${install_dir}" ps -aq control)"
[[ -n "${control_id}" ]] || fail "没有找到 control 容器"
image_ref="$(docker inspect --format '{{.Config.Image}}' "${control_id}")"
[[ -n "${image_ref}" ]] || fail "无法识别 control 镜像"
old_image_id="$(docker image inspect --format '{{.Id}}' "${image_ref}")"
[[ -n "${old_image_id}" ]] || fail "无法识别当前 control 镜像版本"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

binary_name="relay-control-linux-${control_arch}"
say "下载 Relay Control ${version} (${control_arch})"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}" -o "${tmp_dir}/relay-control"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}.sha256" -o "${tmp_dir}/relay-control.sha256"
expected="$(awk '{print $1}' "${tmp_dir}/relay-control.sha256")"
actual="$(sha256sum "${tmp_dir}/relay-control" | awk '{print $1}')"
[[ -n "${expected}" && "${actual}" == "${expected}" ]] || fail "控制端 SHA-256 校验失败"
chmod 0755 "${tmp_dir}/relay-control"

cat > "${tmp_dir}/Dockerfile" <<'EOF'
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

say "构建轻量控制端镜像（不编译源码）"
docker build --quiet --tag "${image_ref}" "${tmp_dir}" >/dev/null
say "规范重建 control 容器"
if ! docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate control >/dev/null; then
  docker tag "${old_image_id}" "${image_ref}"
  docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate control >/dev/null 2>&1 || true
  fail "控制端容器重建失败，已恢复旧镜像"
fi
sleep 3

new_control_id="$(docker compose --project-directory "${install_dir}" ps -q control)"
if [[ -n "${new_control_id}" && "$(docker inspect --format '{{.State.Status}}' "${new_control_id}")" == "running" ]] && docker exec "${new_control_id}" wget -q -O /dev/null http://127.0.0.1:8080/healthz; then
  say "控制端更新完成，数据库、密码和面板配置均未改动"
  exit 0
fi

say "新版启动失败，正在恢复旧版本"
[[ -n "${new_control_id}" ]] && docker logs --tail 30 "${new_control_id}" >&2 || true
docker tag "${old_image_id}" "${image_ref}"
docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate control >/dev/null 2>&1 || true
fail "控制端更新失败，已恢复旧版本"
