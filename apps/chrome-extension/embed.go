// Package chromeextension carries the Chrome extension inside the Go binary,
// so a binary pinned by sha256 (Shipyard's plugin manifest) also pins the
// extension it writes out and checks: see `open-browser-use shipyard`.
package chromeextension

import "embed"

// Files are the extension's shipped files, the same set
// scripts/package-chrome-extension.sh packs (no tests or fixtures).
//
//go:embed manifest.json background.js content-cursor.js content-webmcp-main.js content-webmcp-bridge.js popup.css popup.html popup.js icons/icon-16.png icons/icon-32.png icons/icon-48.png icons/icon-128.png images/cursor-chat.png
var Files embed.FS
