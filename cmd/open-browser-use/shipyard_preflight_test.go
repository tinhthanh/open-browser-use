package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The tests here don't depend on the machine they run on having Chrome or
// a default keychain: they say so themselves where it matters.
func init() {
	chromeInstalled = func() bool { return true }
	keychainProblem = func() string { return "" }
}

func fakePreflight(t *testing.T, chrome bool, keychain string) {
	t.Helper()
	oldChrome, oldKeychain := chromeInstalled, keychainProblem
	chromeInstalled = func() bool { return chrome }
	keychainProblem = func() string { return keychain }
	t.Cleanup(func() { chromeInstalled, keychainProblem = oldChrome, oldKeychain })
}

func TestStatusSaysWhatTheMacLacksFirst(t *testing.T) {
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

	fakePreflight(t, false, "")
	if st := checkShipyard(p); st.Ready || !strings.Contains(st.Problem, "Google Chrome isn't installed") {
		t.Fatalf("no Chrome: %+v", st)
	}
	fakePreflight(t, true, "no default keychain")
	if st := checkShipyard(p); st.Ready || st.Problem != "no default keychain" {
		t.Fatalf("no keychain: %+v", st)
	}
}

func TestStatusNamesTheProfileAsChromeShowsIt(t *testing.T) {
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
	if err := writeEmbeddedExtension(p.ExtensionDir); err != nil {
		t.Fatal(err)
	}
	if err := writeShipyardNativeManifest(p); err != nil {
		t.Fatal(err)
	}
	// Chrome made the agents' profile from --profile-directory and calls it
	// "Your Chrome" in its own windows.
	os.MkdirAll(p.ProfileDir, 0o700)
	ls, _ := json.Marshal(map[string]any{"profile": map[string]any{"info_cache": map[string]any{shipyardProfile: map[string]any{"name": "Your Chrome"}}}})
	os.WriteFile(filepath.Join(p.ChromeRoot, "Local State"), ls, 0o600)
	st := checkShipyard(p)
	if st.Ready || !strings.Contains(st.Problem, `(Chrome shows it as "Your Chrome")`) {
		t.Fatalf("problem should name the profile as Chrome shows it: %+v", st)
	}
	if shipyardWebStoreExtensionID != "" && !strings.Contains(st.Problem, "Chrome Web Store") {
		t.Fatalf("with a Web Store copy, Chrome adds it: no Load unpacked: %q", st.Problem)
	}
}

func TestOnlyGooglesChromeIsInstalled(t *testing.T) {
	ok := "Executable=/Volumes/Google Chrome/Google Chrome.app/Contents/MacOS/Google Chrome\nAuthority=Developer ID Application: Google LLC (EQHXZ8M8AV)\nTeamIdentifier=EQHXZ8M8AV\n"
	if !signedBy(ok, chromeTeamID) {
		t.Fatal("Google's signature")
	}
	if signedBy("TeamIdentifier=ABCDE12345\n", chromeTeamID) || signedBy("Authority=Google (EQHXZ8M8AV)\n", chromeTeamID) {
		t.Fatal("only the team line counts")
	}
}
