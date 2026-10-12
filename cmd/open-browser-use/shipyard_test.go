package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ifuryst/open-browser-use/internal/wire"
)

// fakeShipyardSockets makes checkShipyard and reloadShipyardExtension see
// the given connected extensions instead of the real browser sockets.
func fakeShipyardSockets(t *testing.T, list func() []connectedProfileInfo) {
	t.Helper()
	old := probeShipyardSockets
	probeShipyardSockets = list
	t.Cleanup(func() { probeShipyardSockets = old })
}

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
	fakeShipyardSockets(t, func() []connectedProfileInfo { return nil })
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

func TestChromeProfileDirsIncludesNamedProfiles(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"Default", "Profile 6", "Shipyard Agent", "System Profile", "Guest Profile", "Crashpad"} {
		os.MkdirAll(filepath.Join(root, dir), 0o700)
	}
	for _, dir := range []string{"Shipyard Agent", "System Profile", "Guest Profile"} {
		os.WriteFile(filepath.Join(root, dir, "Preferences"), []byte("{}"), 0o600)
	}
	dirs, err := chromeProfileDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range dirs {
		names = append(names, filepath.Base(d))
	}
	got := strings.Join(names, ",")
	if got != "Default,Profile 6,Shipyard Agent" {
		t.Fatalf("profiles = %s", got)
	}
}

func TestShippedManifestNamesTheBuild(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "extension")
	if err := writeEmbeddedExtension(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	name, _ := m["version_name"].(string)
	if name != shippedVersionName() || !strings.HasPrefix(name, m["version"].(string)+" build ") || len(extensionBuild()) != 12 {
		t.Fatalf("version_name %q, want %q", name, shippedVersionName())
	}
	if m["manifest_version"] != float64(3) || m["background"] == nil {
		t.Fatalf("manifest lost fields: %s", b)
	}
}

func TestStatusAsksToReloadAnOlderRunningExtension(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("chrome plugin setup is for macOS and Linux")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	plugin := filepath.Join(home, "plugins", "chrome")
	os.MkdirAll(plugin, 0o755)
	p, _ := resolveShipyardPaths(plugin)
	writeEmbeddedExtension(p.ExtensionDir)
	writeShipyardNativeManifest(p)
	os.MkdirAll(p.ProfileDir, 0o700)
	prefs, _ := json.Marshal(map[string]any{"extensions": map[string]any{"settings": map[string]any{
		p.ExtensionID: map[string]any{"path": p.ExtensionDir, "manifest": map[string]any{"version": "1.0.0"}},
	}}})
	os.WriteFile(filepath.Join(p.ProfileDir, "Secure Preferences"), prefs, 0o600)

	running := ""
	fakeShipyardSockets(t, func() []connectedProfileInfo {
		return []connectedProfileInfo{
			{ExtensionID: p.ExtensionID, Directory: "Profile 6", VersionName: "other profile"},
			{ExtensionID: p.ExtensionID, Directory: shipyardProfile, VersionName: running},
		}
	})
	if st := checkShipyard(p); st.Ready || st.ExtensionCurrent || !st.Connected || !strings.Contains(st.Problem, "older copy") {
		t.Fatalf("an old running copy must not be ready: %+v", st)
	}
	running = shippedVersionName()
	if st := checkShipyard(p); !st.Ready || !st.ExtensionCurrent || st.Problem != "" {
		t.Fatalf("the shipped copy is running: %+v", st)
	}
}

func TestReloadShipyardExtensionWaitsForTheNewCopy(t *testing.T) {
	dir, err := os.MkdirTemp("", "obu")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	p := shipyardPaths{ExtensionID: "khljlamhjdojjgoaeildaambelafkkei"}
	var running atomic.Value
	running.Store("1.0.0 build old")
	reloads := make(chan string, 4)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			var req map[string]any
			if wire.ReadJSON(conn, &req) == nil {
				method, _ := req["method"].(string)
				running.Store(shippedVersionName())
				reloads <- method
				wire.WriteJSON(conn, map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"reloading": true}})
			}
			conn.Close()
		}
	}()
	fakeShipyardSockets(t, func() []connectedProfileInfo {
		return []connectedProfileInfo{{SocketPath: sock, ExtensionID: p.ExtensionID, Directory: shipyardProfile, VersionName: running.Load().(string)}}
	})
	if err := reloadShipyardExtension(p, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := <-reloads; got != "reloadExtension" {
		t.Fatalf("sent %q", got)
	}
	if err := reloadShipyardExtension(p, time.Second); err != nil || len(reloads) != 0 {
		t.Fatalf("the current copy must not be reloaded again: %v", err)
	}
}

func TestReloadShipyardExtensionExplainsAnOldCopyThatCannotReload(t *testing.T) {
	dir, _ := os.MkdirTemp("", "obu")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "x.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		var req map[string]any
		wire.ReadJSON(conn, &req)
		wire.WriteJSON(conn, map[string]any{"jsonrpc": "2.0", "id": req["id"], "error": map[string]any{"code": -32000, "message": "No handler registered for method: reloadExtension"}})
		conn.Close()
	}()
	p := shipyardPaths{ExtensionID: "khljlamhjdojjgoaeildaambelafkkei"}
	fakeShipyardSockets(t, func() []connectedProfileInfo {
		return []connectedProfileInfo{{SocketPath: sock, ExtensionID: p.ExtensionID, Directory: shipyardProfile}}
	})
	err = reloadShipyardExtension(p, time.Second)
	if err == nil || !strings.Contains(err.Error(), "click reload") {
		t.Fatalf("unexpected: %v", err)
	}
}
