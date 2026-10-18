#!/usr/bin/env bash
# Package open-browser-use as Shipyard's "chrome" plugin: a folder (and a
# .tar.gz of it) whose berth-plugin.json pins the binary (which embeds the
# Chrome extension) and the berth-chrome skill by sha256, with a setup step
# that loads the extension into a dedicated "Shipyard Agent" Chrome profile.
#
#   scripts/package-shipyard-plugin.sh                       # this machine's OS/arch
#   scripts/package-shipyard-plugin.sh --os linux --arch arm64
#   scripts/package-shipyard-plugin.sh --arch universal      # macOS, Apple silicon + Intel
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
    -h|--help) sed -n '2,10p' "$0"; exit 0 ;;
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
if [[ "${arch}" == "universal" && "${os}" != "darwin" ]]; then
  echo "--arch universal is for --os darwin" >&2; exit 1
fi

commit="$(git -C "${repo_root}" rev-parse --short HEAD)"
version="$(git -C "${repo_root}" describe --tags --always 2>/dev/null || echo "${commit}")"
plugin_dir="${out_dir}/chrome"
rm -rf "${plugin_dir}"
mkdir -p "${plugin_dir}/bin" "${plugin_dir}/skills/berth-chrome"

build() { # GOARCH OUT
  (cd "${repo_root}" && CGO_ENABLED=0 GOOS="${os}" GOARCH="$1" go build -trimpath \
    -ldflags "-s -w -X main.version=${version}" -o "$2" ./cmd/open-browser-use)
}
if [[ "${arch}" == "universal" ]]; then
  # One binary for Apple silicon and Intel Macs, so the plugin goes to
  # either kind of box.
  slices="$(mktemp -d)"
  build arm64 "${slices}/arm64"
  build amd64 "${slices}/amd64"
  lipo -create -output "${plugin_dir}/bin/open-browser-use" "${slices}/arm64" "${slices}/amd64"
  # Apple silicon runs only signed code: sign both slices (ad hoc, as the
  # Go linker signs an arm64 binary).
  codesign --force --sign - "${plugin_dir}/bin/open-browser-use"
  rm -rf "${slices}"
else
  build "${arch}" "${plugin_dir}/bin/open-browser-use"
fi
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
    "problem": "problem",
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
