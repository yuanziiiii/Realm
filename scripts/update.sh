#!/usr/bin/env bash
set -euo pipefail

repo="${RELAY_PANEL_REPO:-yuanziiiii/Realm}"
version="${RELAY_PANEL_VERSION:-latest}"

[[ "$(uname -s)" == "Linux" ]] || { printf '[Relay Panel] 错误：在线更新仅支持 Linux\n' >&2; exit 1; }
[[ "${EUID}" -eq 0 ]] || { printf '[Relay Panel] 错误：请使用 root 运行（curl ... | sudo bash）\n' >&2; exit 1; }
command -v curl >/dev/null 2>&1 || { printf '[Relay Panel] 错误：缺少 curl\n' >&2; exit 1; }

if [[ "${version}" == "latest" ]]; then
  update_url="https://github.com/${repo}/releases/latest/download/update-control.sh"
else
  update_url="https://github.com/${repo}/releases/download/${version}/update-control.sh"
fi

tmp_file="$(mktemp)"
trap 'rm -f "${tmp_file}"' EXIT
curl --proto '=https' --tlsv1.2 -fsSL "${update_url}" -o "${tmp_file}"
bash "${tmp_file}"
