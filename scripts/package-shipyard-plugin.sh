#!/usr/bin/env bash
# Package open-browser-use as Shipyard's "chrome" plugin: a folder (and a
# .tar.gz of it) whose berth-plugin.json pins the binary (which embeds the
# Chrome extension) and the berth-chrome skill by sha256, with a setup step
# that loads the extension into a dedicated "Shipyard Agent" Chrome profile.
#
#   scripts/package-shipyard-plugin.sh                       # this machine's OS/arch
#   scripts/package-shipyard-plugin.sh --os linux --arch arm64
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
arch="$(uname -m)"
out_dir="${repo_root}/dist/shipyard-plugin"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --os) os="${2:?--os requires a value}"; shift 2 ;;
    --arch) arch="${2:?--arch requires a value}"; shift 2 ;;
    --out-dir) out_dir="${2:?--out-dir requires a value}"; shift 2 ;;
    -h|--help) sed -n '2,9p' "$0"; exit 0 ;;
    *) echo "Unknown argument: $1" >&2; exit 1 ;;
  esac
done
case "${arch}" in
  x86_64) arch="amd64" ;;
  aarch64) arch="arm64" ;;
esac
case "${os}" in
  darwin|linux) ;;
  *) echo "Unsupported --os ${os} (use darwin or linux)" >&2; exit 1 ;;
esac

commit="$(git -C "${repo_root}" rev-parse --short HEAD)"
version="$(git -C "${repo_root}" describe --tags --always 2>/dev/null || echo "${commit}")"
plugin_dir="${out_dir}/chrome"
rm -rf "${plugin_dir}"
mkdir -p "${plugin_dir}/bin" "${plugin_dir}/skills/berth-chrome"

(cd "${repo_root}" && CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" go build -trimpath \
  -ldflags "-s -w -X main.version=${version}" -o "${plugin_dir}/bin/open-browser-use" ./cmd/open-browser-use)
cp "${repo_root}/plugins/shipyard/chrome/skills/berth-chrome/SKILL.md" "${plugin_dir}/skills/berth-chrome/SKILL.md"

sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
bin_sha="$(sha256 "${plugin_dir}/bin/open-browser-use")"
skill_sha="$(sha256 "${plugin_dir}/skills/berth-chrome/SKILL.md")"

cat > "${plugin_dir}/berth-plugin.json" <<JSON
{
  "name": "Chrome",
  "version": "${version}",
  "description": "Lets agents use real Google Chrome: its own \"Shipyard Agent\" profile, and other profiles only if you turn them on (open-browser-use ${version}, ${os}-${arch}).",
  "mcp": {
    "chrome": {
      "command": "bin/open-browser-use",
      "args": ["mcp", "--browser", "chrome", "--shipyard-profiles"],
      "sha256": "${bin_sha}"
    }
  },
  "skills": {
    "berth-chrome": {
      "path": "skills/berth-chrome/SKILL.md",
      "sha256": "${skill_sha}"
    }
  },
  "setup": {
    "server": "chrome",
    "args": ["shipyard", "setup"],
    "status": ["shipyard", "status", "--json"],
    "ready": "ready",
    "label": "Load the agent's extension in the Chrome profiles agents may use"
  },
  "settings": {
    "server": "chrome",
    "list": ["shipyard", "settings", "--json"],
    "label": "Chrome profiles agents may use"
  }
}
JSON

archive="${out_dir}/chrome-${version}-${os}-${arch}.tar.gz"
tar -C "${out_dir}" -czf "${archive}" chrome
(cd "${out_dir}" && shasum -a 256 "$(basename "${archive}")" > "$(basename "${archive}").sha256")
echo "Plugin folder: ${plugin_dir}"
echo "Archive:       ${archive}"
echo "Binary sha256: ${bin_sha}"
