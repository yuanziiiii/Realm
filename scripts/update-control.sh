#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
requested_version="${RELAY_PANEL_VERSION:-latest}"
install_dir="${RELAY_PANEL_INSTALL_DIR:-/opt/relay-panel}"
version_file="${install_dir}/.relay-panel-version"
force_update="${RELAY_PANEL_FORCE_UPDATE:-0}"

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

resolve_latest_version() {
  local effective
  effective="$(curl --proto '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' "https://github.com/${repo}/releases/latest")"
  [[ "${effective}" == */tag/* ]] || fail "无法识别 GitHub 最新版本"
  printf '%s\n' "${effective##*/}"
}

if [[ "${requested_version}" == "latest" ]]; then
  target_version="$(resolve_latest_version)"
else
  target_version="${requested_version}"
  [[ "${target_version}" == v* ]] || target_version="v${target_version}"
fi
[[ "${target_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]] || fail "无效的目标版本：${target_version}"

current_version="未知"
if [[ -s "${version_file}" ]]; then
  current_version="$(tr -d '[:space:]' < "${version_file}")"
fi
say "当前版本：${current_version}"
say "目标版本：${target_version}"
if [[ "${current_version}" == "${target_version}" && "${force_update}" != "1" ]]; then
  say "已经是最新版本，无需下载镜像或重建容器"
  exit 0
fi

download_base="https://github.com/${repo}/releases/download/${target_version}"
control_id="$(docker compose --project-directory "${install_dir}" ps -aq control)"
web_id="$(docker compose --project-directory "${install_dir}" ps -aq web)"
[[ -n "${control_id}" && -n "${web_id}" ]] || fail "没有找到 control 或 web 容器"
control_image_ref="$(docker inspect --format '{{.Config.Image}}' "${control_id}")"
web_image_ref="$(docker inspect --format '{{.Config.Image}}' "${web_id}")"
old_control_image_id="$(docker image inspect --format '{{.Id}}' "${control_image_ref}")"
old_web_image_id="$(docker image inspect --format '{{.Id}}' "${web_image_ref}")"
[[ -n "${old_control_image_id}" && -n "${old_web_image_id}" ]] || fail "无法识别当前镜像版本"

tmp_dir="$(mktemp -d)"
release_control="relay-panel-control-release:${target_version}-${control_arch}"
release_web="relay-panel-web-release:${target_version}-${control_arch}"
cleanup() {
  docker image rm "${release_control}" "${release_web}" >/dev/null 2>&1 || true
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

download_optional_checked() {
  local name="$1" target="$2" expected actual
  curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${name}" -o "${target}" || return 1
  curl --proto '=https' --tlsv1.2 -fsSL "${download_base}/${name}.sha256" -o "${target}.sha256" || return 1
  expected="$(awk '{print $1}' "${target}.sha256")"
  actual="$(sha256sum "${target}" | awk '{print $1}')"
  [[ -n "${expected}" && "${actual}" == "${expected}" ]] || fail "${name} 的 SHA-256 校验失败"
}

say "发现新版本，下载 GitHub 已预构建的 ${control_arch} 镜像"
download_checked "relay-panel-web-linux-${control_arch}.tar.gz" "${tmp_dir}/web-image.tar.gz"
download_checked "relay-panel-control-linux-${control_arch}.tar.gz" "${tmp_dir}/control-image.tar.gz"
download_checked "relay-panel-source.tar.gz" "${tmp_dir}/source.tar.gz"
zf_available=false
if download_optional_checked "zf.sh" "${tmp_dir}/zf"; then
  chmod 0755 "${tmp_dir}/zf"
  zf_available=true
fi

say "载入预构建镜像（服务器不再执行 npm 或 Go 编译）"
docker load --input "${tmp_dir}/web-image.tar.gz" >/dev/null
docker load --input "${tmp_dir}/control-image.tar.gz" >/dev/null
docker image inspect "${release_web}" >/dev/null 2>&1 || fail "网页端预构建镜像名称不匹配"
docker image inspect "${release_control}" >/dev/null 2>&1 || fail "控制端预构建镜像名称不匹配"
docker tag "${release_web}" "${web_image_ref}"
docker tag "${release_control}" "${control_image_ref}"

say "仅重建 web 与 control 容器，数据库和配置保持不变"
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
  mkdir -p "${tmp_dir}/source"
  tar -xzf "${tmp_dir}/source.tar.gz" -C "${tmp_dir}/source"
  tar -C "${tmp_dir}/source" -cf - . | tar -C "${install_dir}" -xf -
  printf '%s\n' "${target_version}" > "${version_file}"
  if [[ "${zf_available}" == true ]]; then
    install -m 0755 "${tmp_dir}/zf" /usr/local/bin/zf
  fi
  [[ "${old_web_image_id}" == "$(docker image inspect --format '{{.Id}}' "${web_image_ref}")" ]] || docker image rm "${old_web_image_id}" >/dev/null 2>&1 || true
  [[ "${old_control_image_id}" == "$(docker image inspect --format '{{.Id}}' "${control_image_ref}")" ]] || docker image rm "${old_control_image_id}" >/dev/null 2>&1 || true
  say "增量更新完成：${current_version} → ${target_version}"
  say "本次未在服务器编译源码；管理员密码、HTTPS 反代和数据库均未改动"
  [[ "${zf_available}" == true ]] && say "SSH 管理工具已同步更新"
  exit 0
fi

say "新版健康检查失败，正在恢复旧版网页端和控制端"
[[ -n "${new_control_id}" ]] && docker logs --tail 30 "${new_control_id}" >&2 || true
[[ -n "${new_web_id}" ]] && docker logs --tail 30 "${new_web_id}" >&2 || true
docker tag "${old_web_image_id}" "${web_image_ref}"
docker tag "${old_control_image_id}" "${control_image_ref}"
docker compose --project-directory "${install_dir}" up -d --no-deps --no-build --force-recreate web control >/dev/null 2>&1 || true
fail "控制端更新失败，已恢复旧版本"
