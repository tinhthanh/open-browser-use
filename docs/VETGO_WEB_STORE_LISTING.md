# VetGo Browser Agent: Chrome Web Store listing

The values to enter in the Chrome Web Store developer dashboard for the
Shipyard build of this extension. Steps and trade-offs are in
`SHIPYARD_WEB_STORE.md`. Keep this file in step with
`apps/chrome-extension/manifest.json`.

## Package

- Zip: `scripts/package-shipyard-webstore.sh` → `dist/shipyard-webstore/*.zip`
- Visibility: **Unlisted**

## Store listing

**Description**

```text
VetGo Browser Agent lets AI agents running on your own computer (through
Shipyard's chrome plugin) use Chrome: open tabs, read pages as compact
outlines, click and fill forms.

It does nothing on its own. It talks only to a native host that Shipyard's
chrome plugin installs on the same computer, and it works in a Chrome profile
only after you:
1. add the extension to that profile,
2. turn it on there, and
3. allow that profile in Shipyard (Settings → Plugins → Chrome).

Every profile is off by default, so profiles you keep for banking or personal
mail stay out of reach unless you turn them on.

The extension sends nothing to a VetGo, Shipyard or other cloud service. It is
built from the open-source Open Browser Use project (MIT):
https://github.com/tinhthanh/open-browser-use
```

- Category: **Developer Tools**
- Language: **English**
- Screenshot: `docs/webstore/screenshot-1280x800.png`
- Small promo tile: `docs/webstore/promo-tile-440x280.png`
- Homepage URL: `https://github.com/tinhthanh/open-browser-use`
- Support URL: `https://github.com/tinhthanh/open-browser-use/issues`

## Privacy practices

**Single purpose**

```text
Lets AI agents running on the user's own computer, through a locally installed
native host, control Chrome tabs in the profiles the user allowed.
```

**Permission justifications**

| Permission | Justification |
| --- | --- |
| `alarms` | Reconnects to the local native host and sends a heartbeat after Chrome suspends the service worker. |
| `debugger` | Runs Chrome DevTools Protocol commands the local agent asks for: navigate, read the page outline, click, type, take screenshots. |
| `downloads` | Reports files downloaded during an agent session back to the local agent. |
| `downloads.ui` | Shows Chrome's download UI for files an agent session downloads. |
| `history` | Answers history searches only when the local agent explicitly asks for them. |
| `nativeMessaging` | Connects to the native host installed on the same computer; it is the extension's only channel. |
| `scripting` | Injects the cursor overlay script into tabs an agent controls, so the user sees where it acts. |
| `storage` | Keeps the native host status, the extension instance id and agent session state across service worker restarts. |
| `tabGroups` | Groups the tabs an agent opens, so they are separate from the user's own tabs. |
| `tabs` | Opens, lists and identifies the tabs an agent session uses. |
| Host permission `<all_urls>` | The user decides which sites the agent works on, so the extension must be able to act on any site the user sends it to. |

**Remote code**: No, the extension does not use remote code. All of its code
ships in the package.

**Data usage**: tick the kinds of data the extension can handle while an agent
uses an allowed profile: personally identifiable information, health
information, financial and payment information, authentication information,
personal communications, location, web history, user activity, website
content. Then certify:

- not sold to third parties,
- not used or transferred for purposes unrelated to the item's single purpose,
- not used or transferred to determine creditworthiness or for lending.

**Privacy policy URL**:
`https://github.com/tinhthanh/open-browser-use/blob/main/docs/VETGO_PRIVACY_POLICY.md`
