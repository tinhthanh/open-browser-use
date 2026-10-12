# Publishing the Shipyard extension on the Chrome Web Store

Shipyard's chrome plugin works with an unpacked copy of the extension, which
the person loads by hand in each Chrome profile (Developer mode → Load
unpacked). A Chrome Web Store copy removes that step: once the plugin knows
its id, setup asks Chrome to add the extension to **every** profile, turned
off, and the person only turns it on in the profiles they also allow in
Shipyard (Settings → Plugins → Chrome). Chrome 137+ offers no other way to
add an extension we build ourselves to a profile on macOS or Windows.

## What changes with a Web Store copy

- **The extension is in every profile**, banking ones included, turned off
  until the person turns it on there. Chrome can't be told which profiles
  get it.
- **Updates go through Google's review** (hours to days). The plugin only
  uses a copy whose `version_name` is the one its binary ships (`<version>
  build <digest>`), so a plugin update leaves Web Store profiles unusable
  until the matching extension version is live.
- **The binary's sha256 no longer pins the code Chrome runs** in those
  profiles; the `version_name` check does instead.
- Developer mode is no longer needed in those profiles.

## Steps

1. Pick a listing name of your own. Don't reuse upstream's "Open Browser
   Use" listing; this fork's extension adds code (the navigation guard, the
   reload method) that upstream's copy lacks.
2. Build the zip from the commit you package the plugin from:
   `scripts/package-shipyard-webstore.sh` → `dist/shipyard-webstore/*.zip`.
3. Upload it as a new item in the Chrome Web Store developer dashboard
   (visibility **Unlisted** is enough). Reuse the justifications in
   `CHROME_WEB_STORE_LISTING.md` for `debugger`, `nativeMessaging` and host
   permissions, and the policy in `PRIVACY_POLICY.md`.
4. After the first upload, copy the item's id into
   `shipyardWebStoreExtensionID` in `cmd/open-browser-use/shipyard_profiles.go`,
   then package and install the plugin again. Setup now writes
   `External Extensions/<id>.json` for Chrome, and the native host accepts
   both the unpacked and the Web Store copy.
5. For each later plugin release, upload the zip built from the same
   commit.
