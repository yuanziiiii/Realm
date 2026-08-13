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

tmp_dir="$(mktemp -d)"
restart_needed=false
cleanup() {
  rm -rf "${tmp_dir}"
  if [[ "${restart_needed}" == true ]]; then
    docker start "${control_id}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

binary_name="relay-control-linux-${control_arch}"
say "下载 Relay Control ${version} (${control_arch})"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}" -o "${tmp_dir}/relay-control"
curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${binary_name}.sha256" -o "${tmp_dir}/relay-control.sha256"
expected="$(awk '{print $1}' "${tmp_dir}/relay-control.sha256")"
actual="$(sha256sum "${tmp_dir}/relay-control" | awk '{print $1}')"
[[ -n "${expected}" && "${actual}" == "${expected}" ]] || fail "控制端 SHA-256 校验失败"
chmod 0755 "${tmp_dir}/relay-control"

say "备份当前控制端并更新程序"
docker cp "${control_id}:/usr/local/bin/relay-control" "${tmp_dir}/relay-control.previous"
docker compose --project-directory "${install_dir}" stop control >/dev/null
restart_needed=true
docker cp "${tmp_dir}/relay-control" "${control_id}:/usr/local/bin/relay-control"
docker commit "${control_id}" "${image_ref}" >/dev/null
docker start "${control_id}" >/dev/null
sleep 3

if [[ "$(docker inspect --format '{{.State.Status}}' "${control_id}")" == "running" ]] && docker exec "${control_id}" wget -q -O /dev/null http://127.0.0.1:8080/healthz; then
  restart_needed=false
  say "控制端更新完成，数据库、密码和面板配置均未改动"
  exit 0
fi

say "新版启动失败，正在恢复旧版本"
docker stop "${control_id}" >/dev/null 2>&1 || true
docker cp "${tmp_dir}/relay-control.previous" "${control_id}:/usr/local/bin/relay-control"
docker commit "${control_id}" "${image_ref}" >/dev/null
docker start "${control_id}" >/dev/null 2>&1 || true
restart_needed=false
docker logs --tail 30 "${control_id}" >&2 || true
fail "控制端更新失败，已恢复旧版本"
