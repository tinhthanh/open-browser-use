package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shipyardProfilesHome makes a HOME with a Chrome holding "Shipyard Agent",
// "Default" (Personal) and "Profile 6" (Bank), the plugin in it, and a
// settings file; it returns the plugin's paths and a writer for settings.
func shipyardProfilesHome(t *testing.T) (shipyardPaths, func(values map[string]bool)) {
	t.Helper()
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
	for _, dir := range []string{shipyardProfile, "Default", "Profile 6"} {
		os.MkdirAll(filepath.Join(p.ChromeRoot, dir), 0o700)
		os.WriteFile(filepath.Join(p.ChromeRoot, dir, "Preferences"), []byte("{}"), 0o600)
	}
	os.WriteFile(filepath.Join(p.ChromeRoot, "Local State"), []byte(`{"profile":{"info_cache":{"Default":{"name":"Personal"},"Profile 6":{"name":"Bank"}}}}`), 0o600)
	settings := filepath.Join(home, "plugin-settings", "chrome.json")
	t.Setenv("BERTH_PLUGIN_SETTINGS", settings)
	write := func(values map[string]bool) {
		os.MkdirAll(filepath.Dir(settings), 0o700)
		b, _ := json.Marshal(map[string]any{"values": values})
		os.WriteFile(settings, b, 0o600)
	}
	return p, write
}

func installInProfile(t *testing.T, p shipyardPaths, dir string, entry map[string]any) {
	t.Helper()
	entry["path"] = p.ExtensionDir
	entry["manifest"] = map[string]any{"version": "1.0.0"}
	prefs, _ := json.Marshal(map[string]any{"extensions": map[string]any{"settings": map[string]any{p.ExtensionID: entry}}})
	os.WriteFile(filepath.Join(p.ChromeRoot, dir, "Secure Preferences"), prefs, 0o600)
}

func TestOnlyTheAgentProfileIsAllowedUntilTurnedOn(t *testing.T) {
	p, write := shipyardProfilesHome(t)
	got := map[string]bool{}
	var order []string
	for _, ap := range agentProfiles(p) {
		got[ap.Name] = ap.Allowed
		order = append(order, ap.Name)
	}
	if strings.Join(order, ",") != "Shipyard Agent,Bank,Personal" {
		t.Fatalf("order %v", order)
	}
	if !got[shipyardProfile] || got["Bank"] || got["Personal"] {
		t.Fatalf("defaults: %v", got)
	}
	write(map[string]bool{"profile:Shipyard Agent": false, "profile:Default": true})
	allowed := allowedAgentProfiles(p)
	if len(allowed) != 1 || allowed[0].Dir != "Default" {
		t.Fatalf("allowed %+v", allowed)
	}
}

func TestSettingsListWarnsBeforeTheUsersOwnProfiles(t *testing.T) {
	p, _ := shipyardProfilesHome(t)
	installInProfile(t, p, "Profile 6", map[string]any{"disable_reasons": []int{1}})
	byKey := map[string]pluginSetting{}
	for _, s := range shipyardSettingsList(p) {
		byKey[s.Key] = s
	}
	agent, bank, personal := byKey["profile:Shipyard Agent"], byKey["profile:Profile 6"], byKey["profile:Default"]
	if !agent.Value || agent.Warning != "" {
		t.Fatalf("agent profile: %+v", agent)
	}
	if bank.Value || bank.Label != "Bank (Profile 6)" || !strings.Contains(bank.Warning, "signed in") || !strings.Contains(bank.Description, "turned off in Chrome") {
		t.Fatalf("bank profile: %+v", bank)
	}
	if !strings.Contains(personal.Description, "not added") {
		t.Fatalf("personal profile: %+v", personal)
	}
}

func TestStatusNeedsTheExtensionInEveryAllowedProfile(t *testing.T) {
	p, write := shipyardProfilesHome(t)
	writeEmbeddedExtension(p.ExtensionDir)
	writeShipyardNativeManifest(p)
	installInProfile(t, p, shipyardProfile, map[string]any{})
	if st := checkShipyard(p); !st.Ready {
		t.Fatalf("only the agent profile is allowed and set up: %+v", st)
	}
	write(map[string]bool{"profile:Default": true})
	if st := checkShipyard(p); st.Ready || !strings.Contains(st.Problem, `"Personal"`) {
		t.Fatalf("Personal is allowed but lacks the extension: %+v", st)
	}
	installInProfile(t, p, "Default", map[string]any{"state": 0})
	if st := checkShipyard(p); st.Ready || !strings.Contains(st.Problem, "turn on VetGo Browser Agent") {
		t.Fatalf("Personal has it turned off in Chrome: %+v", st)
	}
	installInProfile(t, p, "Default", map[string]any{"state": 1})
	if st := checkShipyard(p); !st.Ready {
		t.Fatalf("both allowed profiles are set up: %+v", st)
	}
	write(map[string]bool{"profile:Shipyard Agent": false})
	if st := checkShipyard(p); st.Ready || !strings.Contains(st.Problem, "no Chrome profile") {
		t.Fatalf("nothing allowed: %+v", st)
	}
}

func TestMCPUsesOnlyAllowedProfiles(t *testing.T) {
	p, write := shipyardProfilesHome(t)
	t.Setenv("BERTH_PLUGIN_DIR", p.PluginDir)
	server := newMCPServer(socketOptions{})
	server.shipyardProfiles = true

	if err := server.selectShipyardProfile(); err != nil || server.runner.options.profile != "dir:Shipyard Agent" || server.runner.options.browser != "chrome" {
		t.Fatalf("default: %v %q", err, server.runner.options.profile)
	}
	if _, err := server.useProfileTool(map[string]any{"profile": "Bank"}); err == nil {
		t.Fatal("a profile the user did not turn on must be refused")
	}
	write(map[string]bool{"profile:Profile 6": true})
	if out, err := server.useProfileTool(map[string]any{"profile": "bank"}); err != nil || server.runner.options.profile != "dir:Profile 6" {
		t.Fatalf("use_profile Bank: %v %v %q", out, err, server.runner.options.profile)
	}
	write(map[string]bool{})
	if err := server.selectShipyardProfile(); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Fatalf("a profile turned off mid-session must stop being used: %v", err)
	}

	server = newMCPServer(socketOptions{})
	server.shipyardProfiles = true
	write(map[string]bool{"profile:Shipyard Agent": false, "profile:Default": true, "profile:Profile 6": true})
	if err := server.selectShipyardProfile(); err == nil || !strings.Contains(err.Error(), "use_profile") {
		t.Fatalf("two allowed and no default: %v", err)
	}
	write(map[string]bool{"profile:Shipyard Agent": false})
	if err := server.selectShipyardProfile(); err == nil || !strings.Contains(err.Error(), "no Chrome profile") {
		t.Fatalf("nothing allowed: %v", err)
	}
	names := []string{}
	for _, tool := range server.tools() {
		names = append(names, tool.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "use_profile") {
		t.Fatal("use_profile must be offered")
	}
}

func TestDirSelectorIgnoresDisplayNames(t *testing.T) {
	impostor := connectedProfileInfo{Directory: "Profile 6", DisplayName: "Shipyard Agent"}
	if profileSelectorMatches("dir:Shipyard Agent", impostor) {
		t.Fatal("a profile renamed to look like the agent's must not match")
	}
	if !profileSelectorMatches("dir:Shipyard Agent", connectedProfileInfo{Directory: "Shipyard Agent"}) {
		t.Fatal("the folder must match")
	}
	if !profileSelectorMatches("Shipyard Agent", impostor) {
		t.Fatal("plain selectors keep matching display names")
	}
}

func TestWebStoreCopyIsAddedToEveryProfileAndAccepted(t *testing.T) {
	p, _ := shipyardProfilesHome(t)
	old := shipyardWebStoreExtensionID
	shipyardWebStoreExtensionID = "abcdefghijklmnopabcdefghijklmnop"
	t.Cleanup(func() { shipyardWebStoreExtensionID = old })
	if err := writeWebStoreExternalExtension(p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(p.ChromeRoot, "External Extensions", shipyardWebStoreExtensionID+".json"))
	if err != nil || !strings.Contains(string(b), "clients2.google.com/service/update2/crx") {
		t.Fatalf("external extension file: %s %v", b, err)
	}
	writeShipyardNativeManifest(p)
	m, _ := os.ReadFile(p.Manifest)
	if !strings.Contains(string(m), p.ExtensionID) || !strings.Contains(string(m), shipyardWebStoreExtensionID) {
		t.Fatalf("native host must accept both copies: %s", m)
	}
	if !checkShipyard(p).NativeHost {
		t.Fatal("status must accept the two-origin manifest")
	}
}
