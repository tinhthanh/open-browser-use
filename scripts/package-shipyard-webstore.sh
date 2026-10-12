#!/usr/bin/env bash
# Zip the extension exactly as Shipyard's chrome plugin ships it (manifest
# version_name "<version> build <digest>"), for a Chrome Web Store upload.
# The plugin accepts a Web Store copy only when its version_name matches the
# binary's, so upload the zip built from the commit you package the plugin from.
#
#   scripts/package-shipyard-webstore.sh
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${repo_root}/dist/shipyard-webstore"
stage="$(mktemp -d)"
trap 'rm -rf "${stage}"' EXIT

version_name="$(cd "${repo_root}" && go run ./cmd/open-browser-use shipyard write-extension "${stage}/extension")"
mkdir -p "${out_dir}"
zip_path="${out_dir}/open-browser-use-shipyard-${version_name// /-}.zip"
rm -f "${zip_path}"
(cd "${stage}/extension" && zip -qr -X "${zip_path}" .)
echo "Extension: ${version_name}"
echo "Zip:       ${zip_path}"
