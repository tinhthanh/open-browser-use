package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Which Chrome profiles agents may use is a Shipyard setting of the chrome
// plugin (Settings → Plugins → Chrome), one switch per profile. Shipyard
// keeps the values outside the plugin's folder and passes their file in
// $BERTH_PLUGIN_SETTINGS; `shipyard settings --json` lists the switches.
// The "Shipyard Agent" profile is on unless turned off; every other profile
// is off until the person turns it on.

// shipyardWebStoreExtensionID is the extension's Chrome Web Store id once
// it is published there. Then setup also asks Chrome to add it to every
// profile (turned off until the person turns it on in that profile), and
// the native host accepts that copy too. Empty: unpacked copies only.
var shipyardWebStoreExtensionID = ""

func profileSettingKey(dir string) string { return "profile:" + dir }

// readShipyardSettings reads the values Shipyard stored, if any.
func readShipyardSettings() map[string]bool {
	doc := struct {
		Values map[string]bool `json:"values"`
	}{}
	if path := os.Getenv("BERTH_PLUGIN_SETTINGS"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &doc)
		}
	}
	return doc.Values
}

// agentProfile is a Chrome profile as the chrome plugin sees it.
type agentProfile struct {
	Dir     string `json:"dir"`
	Name    string `json:"name"`
	Allowed bool   `json:"allowed"`
	// Installed: the extension is in this profile; Off: but turned off in
	// Chrome (as Chrome adds a Web Store copy to every profile).
	Installed bool `json:"installed"`
	Off       bool `json:"off,omitempty"`
}

// shipyardExtensionIDs are the ids the plugin's extension can have: the
// unpacked copy's, and the Web Store copy's once there is one.
func (p shipyardPaths) shipyardExtensionIDs() []string {
	ids := []string{p.ExtensionID}
	if shipyardWebStoreExtensionID != "" {
		ids = append(ids, shipyardWebStoreExtensionID)
	}
	return ids
}

func (p shipyardPaths) isShipyardExtensionID(id string) bool {
	for _, want := range p.shipyardExtensionIDs() {
		if id == want {
			return true
		}
	}
	return false
}

// agentProfiles lists this computer's Chrome profiles, "Shipyard Agent"
// first (even before Chrome created it), then the others by name.
func agentProfiles(p shipyardPaths) []agentProfile {
	values := readShipyardSettings()
	names := chromeProfileDisplayNames(p.ChromeRoot)
	dirs, _ := chromeProfileDirs(p.ChromeRoot)
	build := func(dir string) agentProfile {
		ap := agentProfile{Dir: dir, Name: names[dir]}
		// Chrome names a new profile "Your Chrome"; the agents' own one is
		// known by its folder.
		if ap.Name == "" || dir == shipyardProfile {
			ap.Name = dir
		}
		on, set := values[profileSettingKey(dir)]
		ap.Allowed = on || (!set && dir == shipyardProfile)
		for _, id := range p.shipyardExtensionIDs() {
			if installed, off := extensionStateInProfile(filepath.Join(p.ChromeRoot, dir), id); installed {
				ap.Installed, ap.Off = true, off
				break
			}
		}
		return ap
	}
	var rest []agentProfile
	for _, d := range dirs {
		if dir := filepath.Base(d); dir != shipyardProfile {
			rest = append(rest, build(dir))
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return strings.ToLower(rest[i].Name) < strings.ToLower(rest[j].Name) })
	return append([]agentProfile{build(shipyardProfile)}, rest...)
}

// allowedAgentProfiles are the profiles agents may use.
func allowedAgentProfiles(p shipyardPaths) []agentProfile {
	var out []agentProfile
	for _, ap := range agentProfiles(p) {
		if ap.Allowed {
			out = append(out, ap)
		}
	}
	return out
}

// extensionStateInProfile says whether the extension is in a profile, and
// whether Chrome has it turned off there.
func extensionStateInProfile(profileDir, id string) (installed, off bool) {
	if _, ok := detectUnpackedExtensionInProfile(profileDir, id); !ok {
		return false, false
	}
	b, err := os.ReadFile(filepath.Join(profileDir, "Secure Preferences"))
	if err != nil {
		return true, false
	}
	var prefs struct {
		Extensions struct {
			Settings map[string]struct {
				State          *int            `json:"state"`
				DisableReasons json.RawMessage `json:"disable_reasons"`
			} `json:"settings"`
		} `json:"extensions"`
	}
	if json.Unmarshal(b, &prefs) != nil {
		return true, false
	}
	e := prefs.Extensions.Settings[id]
	reasons := strings.TrimSpace(string(e.DisableReasons))
	return true, (e.State != nil && *e.State == 0) || (reasons != "" && reasons != "[]" && reasons != "0" && reasons != "null")
}

// pluginSetting is one switch, in the shape Shipyard's settings list takes.
type pluginSetting struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Value       bool   `json:"value"`
	Warning     string `json:"warning,omitempty"`
}

func shipyardSettingsList(p shipyardPaths) []pluginSetting {
	var out []pluginSetting
	for _, ap := range agentProfiles(p) {
		s := pluginSetting{Key: profileSettingKey(ap.Dir), Label: ap.Name, Value: ap.Allowed}
		if ap.Name != ap.Dir {
			s.Label = fmt.Sprintf("%s (%s)", ap.Name, ap.Dir)
		}
		switch {
		case ap.Dir == shipyardProfile && !ap.Installed:
			s.Description = "The agents' own profile: set up adds the extension here"
		case ap.Dir == shipyardProfile:
			s.Description = "The agents' own profile"
		case !ap.Installed:
			s.Description = "Extension not added to this profile yet: turning it on opens the steps"
		case ap.Off:
			s.Description = "Extension added but turned off in Chrome: turn it on in this profile's chrome://extensions too"
		default:
			s.Description = "Extension added"
		}
		if ap.Dir != shipyardProfile {
			s.Warning = fmt.Sprintf("Agents will be able to act as you on every site you are signed in to in the %q Chrome profile: read your mail, use your accounts, submit forms. Turn this on only for a profile you would hand to an assistant.", ap.Name)
		}
		out = append(out, s)
	}
	return out
}
