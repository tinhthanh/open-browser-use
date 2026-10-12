package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	chromeextension "github.com/ifuryst/open-browser-use/apps/chrome-extension"
	"github.com/ifuryst/open-browser-use/internal/host"
	"github.com/spf13/cobra"
)

// Shipyard's "chrome" plugin runs this binary from its own folder
// ($BERTH_PLUGIN_DIR). `shipyard setup` writes the extension out of the
// binary into <plugin>/extension (so the sha256 that pins the binary pins the
// extension too), registers the native host for exactly that unpacked
// extension, and opens a dedicated Chrome profile with a guide and
// chrome://extensions, because branded Chrome no longer loads extensions from
// the command line. `shipyard status --json` reports whether that is done.

// shipyardProfile is the Chrome profile agents drive: a profile of its own,
// so they never act in the user's everyday or banking profiles.
const shipyardProfile = "Shipyard Agent"

type shipyardPaths struct {
	PluginDir    string
	ExtensionDir string
	ExtensionID  string
	Binary       string
	Manifest     string
	ProfileDir   string
	// ChromeRoot is Chrome's user data folder, holding every profile.
	ChromeRoot string
}

func resolveShipyardPaths(pluginDir string) (shipyardPaths, error) {
	if pluginDir == "" {
		pluginDir = os.Getenv("BERTH_PLUGIN_DIR")
	}
	if pluginDir == "" || !filepath.IsAbs(pluginDir) {
		return shipyardPaths{}, errors.New("run this from Shipyard's chrome plugin (BERTH_PLUGIN_DIR), or pass --plugin-dir with an absolute path")
	}
	if resolved, err := filepath.EvalSymlinks(pluginDir); err == nil {
		pluginDir = resolved
	}
	exe, err := os.Executable()
	if err != nil {
		return shipyardPaths{}, err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	manifest, err := defaultNativeHostManifestPathForBrowser("chrome")
	if err != nil {
		return shipyardPaths{}, err
	}
	root, err := browserRootForInstallSelector("chrome")
	if err != nil {
		return shipyardPaths{}, err
	}
	ext := filepath.Join(pluginDir, "extension")
	return shipyardPaths{
		ChromeRoot:   root.Root,
		PluginDir:    pluginDir,
		ExtensionDir: ext,
		ExtensionID:  unpackedExtensionIDForPath(ext),
		Binary:       exe,
		Manifest:     manifest,
		ProfileDir:   filepath.Join(root.Root, shipyardProfile),
	}, nil
}

// unpackedExtensionIDForPath is the id Chrome gives an unpacked extension
// with no "key": the first 128 bits of sha256(absolute path), as letters a-p.
func unpackedExtensionIDForPath(path string) string {
	sum := sha256.Sum256([]byte(path))
	encoded := hex.EncodeToString(sum[:16])
	var b strings.Builder
	for _, c := range encoded {
		if c >= '0' && c <= '9' {
			b.WriteRune('a' + (c - '0'))
		} else {
			b.WriteRune('k' + (c - 'a'))
		}
	}
	return b.String()
}

// writeEmbeddedExtension makes dir hold exactly the embedded extension.
// The running extension is told apart from the files on disk by the
// version_name setup writes into its manifest.json: Chrome reads the manifest
// only when it loads or reloads the extension, so the version_name the
// extension reports names the code Chrome is running, even after setup wrote
// newer files that Chrome has not loaded yet.

// extensionBuild is a short digest of the extension files this binary ships.
func extensionBuild() string {
	var names []string
	_ = fs.WalkDir(chromeextension.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && name != "manifest.json" {
			names = append(names, name)
		}
		return nil
	})
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		b, _ := chromeextension.Files.ReadFile(name)
		sum := sha256.Sum256(b)
		fmt.Fprintf(h, "%s %x\n", name, sum)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// shippedExtensionName is the name Chrome shows for the shipped extension.
func shippedExtensionName() string {
	var m struct {
		Name string `json:"name"`
	}
	b, _ := chromeextension.Files.ReadFile("manifest.json")
	_ = json.Unmarshal(b, &m)
	return m.Name
}

// shippedVersionName is the version_name of the extension this binary ships.
func shippedVersionName() string {
	var m struct {
		Version string `json:"version"`
	}
	b, _ := chromeextension.Files.ReadFile("manifest.json")
	_ = json.Unmarshal(b, &m)
	return m.Version + " build " + extensionBuild()
}

// shippedFile is an extension file as setup writes it: the embedded file, and
// for manifest.json the embedded manifest with shippedVersionName added.
func shippedFile(name string) ([]byte, error) {
	b, err := chromeextension.Files.ReadFile(name)
	if err != nil || name != "manifest.json" {
		return b, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["version_name"] = shippedVersionName()
	out, err := json.MarshalIndent(m, "", "  ")
	return append(out, '\n'), err
}

func writeEmbeddedExtension(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return fs.WalkDir(chromeextension.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "embed.go" {
			return err
		}
		b, err := shippedFile(name)
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, b, 0o644)
	})
}

// extensionIntact says whether dir holds exactly the embedded extension: a
// changed, missing or added file means it is not the extension that was
// allowed, and setup must write it again.
func extensionIntact(dir string) (bool, string) {
	want := map[string][]byte{}
	_ = fs.WalkDir(chromeextension.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			want[name], _ = shippedFile(name)
		}
		return nil
	})
	seen := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		b, ok := want[rel]
		if !ok {
			return fmt.Errorf("unexpected file %s", rel)
		}
		got, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, b) {
			return fmt.Errorf("%s was changed", rel)
		}
		seen++
		return nil
	})
	if err != nil {
		return false, err.Error()
	}
	if seen != len(want) {
		return false, "files are missing"
	}
	return true, ""
}

func writeShipyardNativeManifest(p shipyardPaths) error {
	manifest, err := nativeManifest(p.ExtensionID, p.Binary)
	if err != nil {
		return err
	}
	manifest["allowed_origins"] = shipyardOrigins(p)
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.Manifest), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.Manifest, append(payload, '\n'), 0o600)
}

func shipyardOrigins(p shipyardPaths) []string {
	var out []string
	for _, id := range p.shipyardExtensionIDs() {
		out = append(out, "chrome-extension://"+id+"/")
	}
	return out
}

// writeWebStoreExternalExtension asks Chrome to add the Web Store copy to
// every profile: Chrome adds it turned off and lets the person turn it on.
func writeWebStoreExternalExtension(p shipyardPaths) error {
	if shipyardWebStoreExtensionID == "" {
		return nil
	}
	_, err := installChromeExternalExtensionForBrowser(shipyardWebStoreExtensionID, filepath.Join(p.ChromeRoot, "External Extensions", shipyardWebStoreExtensionID+".json"), "chrome")
	return err
}

type shipyardStatus struct {
	Ready              bool `json:"ready"`
	NativeHost         bool `json:"nativeHost"`
	Profile            bool `json:"profile"`
	ExtensionInstalled bool `json:"extensionInstalled"`
	ExtensionIntact    bool `json:"extensionIntact"`
	Connected          bool `json:"connected"`
	// ExtensionCurrent: Chrome runs the extension this plugin ships (only
	// known while Chrome is open and the extension is connected).
	ExtensionCurrent bool   `json:"extensionCurrent"`
	RunningVersion   string `json:"runningVersion,omitempty"`
	ProfileName      string `json:"profileName"`
	ExtensionID      string `json:"extensionId"`
	ExtensionDir     string `json:"extensionDir"`
	// Profiles are the Chrome profiles agents may use (Shipyard's settings).
	Profiles []agentProfile `json:"profiles"`
	Problem  string         `json:"problem,omitempty"`
}

// checkShipyard reports whether every profile agents may use is set up:
// the extension intact, the native host registered, the extension added to
// each allowed profile, and Chrome running the shipped copy where it is open.
func checkShipyard(p shipyardPaths) shipyardStatus {
	st := shipyardStatus{ProfileName: shipyardProfile, ExtensionID: p.ExtensionID, ExtensionDir: p.ExtensionDir}
	if b, err := os.ReadFile(p.Manifest); err == nil {
		var m struct {
			Path           string   `json:"path"`
			AllowedOrigins []string `json:"allowed_origins"`
		}
		if json.Unmarshal(b, &m) == nil && m.Path == p.Binary && strings.Join(m.AllowedOrigins, " ") == strings.Join(shipyardOrigins(p), " ") {
			st.NativeHost = true
		}
	}
	if info, err := os.Stat(p.ProfileDir); err == nil && info.IsDir() {
		st.Profile = true
	}
	st.Profiles = allowedAgentProfiles(p)
	var missing, off *agentProfile
	for i := range st.Profiles {
		ap := &st.Profiles[i]
		if !ap.Installed && missing == nil {
			missing = ap
		}
		if ap.Installed && ap.Off && off == nil {
			off = ap
		}
	}
	st.ExtensionInstalled = len(st.Profiles) > 0 && missing == nil
	var why string
	st.ExtensionIntact, why = extensionIntact(p.ExtensionDir)
	connected := connectedShipyardExtensions(p)
	st.Connected = len(connected) > 0
	st.ExtensionCurrent = st.Connected
	for _, c := range connected {
		if c.VersionName != shippedVersionName() {
			st.ExtensionCurrent, st.RunningVersion = false, c.VersionName
		} else if st.RunningVersion == "" {
			st.RunningVersion = c.VersionName
		}
	}
	st.Ready = st.NativeHost && st.ExtensionInstalled && st.ExtensionIntact && off == nil && (!st.Connected || st.ExtensionCurrent)
	switch {
	case !st.ExtensionIntact:
		st.Problem = "the extension folder is not the one this plugin ships (" + why + "); run setup again"
	case !st.NativeHost:
		st.Problem = "the native host is not registered for this extension; run setup again"
	case len(st.Profiles) == 0:
		st.Problem = "no Chrome profile is turned on for agents (Settings → Plugins → Chrome)"
	case missing != nil && missing.Dir == shipyardProfile && !st.Profile:
		st.Problem = "Chrome has no \"" + shipyardProfile + "\" profile yet"
	case missing != nil:
		st.Problem = "load the extension in the \"" + missing.Name + "\" profile (chrome://extensions, Developer mode, Load unpacked)"
	case off != nil:
		st.Problem = "turn on " + shippedExtensionName() + " in chrome://extensions of the \"" + off.Name + "\" profile"
	case st.Connected && !st.ExtensionCurrent:
		st.Problem = "Chrome still runs an older copy of the extension; run setup again to reload it"
	}
	return st
}

// probeShipyardSockets lists the connected extensions (a variable for tests).
var probeShipyardSockets = func() []connectedProfileInfo { return probeConnectedProfiles(host.DefaultSocketDir) }

// connectedShipyardExtensions are the plugin's extensions connected in the
// profiles agents may use.
func connectedShipyardExtensions(p shipyardPaths) []connectedProfileInfo {
	allowed := map[string]bool{}
	for _, ap := range allowedAgentProfiles(p) {
		allowed[ap.Dir] = true
	}
	var out []connectedProfileInfo
	for _, c := range probeShipyardSockets() {
		if p.isShipyardExtensionID(c.ExtensionID) && allowed[c.Directory] {
			out = append(out, c)
		}
	}
	return out
}

// reloadShipyardExtension makes Chrome load the extension this binary ships
// in each allowed profile that still runs an older unpacked copy, and waits
// until the new ones connect. With Chrome closed there is nothing to do:
// Chrome loads the files on disk when it starts. A Web Store copy updates
// through the store instead.
func reloadShipyardExtension(p shipyardPaths, wait time.Duration) error {
	var stale []connectedProfileInfo
	for _, c := range connectedShipyardExtensions(p) {
		if c.VersionName == shippedVersionName() {
			continue
		}
		if c.ExtensionID != p.ExtensionID {
			return fmt.Errorf("the %q profile runs a Web Store copy of the extension (%q) that is not the one this plugin ships; update the plugin or wait for Chrome to update the extension", profileLabel(c), c.VersionName)
		}
		stale = append(stale, c)
	}
	if len(stale) == 0 {
		return nil
	}
	for _, c := range stale {
		response, err := invokeWithProfile(c.SocketPath, "", "", "", "reloadExtension", map[string]any{}, 5*time.Second)
		if err == nil {
			err = responseError("reloadExtension", response)
		}
		if err != nil {
			return fmt.Errorf("Chrome runs an older copy of the extension that cannot reload itself: in the %q profile open chrome://extensions and click reload on %s (%v)", profileLabel(c), shippedExtensionName(), err)
		}
	}
	for deadline := time.Now().Add(wait); time.Now().Before(deadline); {
		time.Sleep(500 * time.Millisecond)
		current := map[string]bool{}
		for _, c := range connectedShipyardExtensions(p) {
			current[c.Directory] = c.VersionName == shippedVersionName()
		}
		done := true
		for _, c := range stale {
			done = done && current[c.Directory]
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("the extension did not reconnect within %s after reloading; check %s in chrome://extensions of the %q profile", wait, shippedExtensionName(), profileLabel(stale[0]))
}

func profileLabel(c connectedProfileInfo) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.Directory
}

// openShipyardGuide opens a profile with the guide and chrome://extensions.
func openShipyardGuide(p shipyardPaths, ap agentProfile) error {
	guide := filepath.Join(p.PluginDir, "setup.html")
	if err := os.WriteFile(guide, []byte(shipyardGuideHTML(p, ap.Name)), 0o644); err != nil {
		return err
	}
	guideURL := "file://" + guide
	var chrome *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// --profile-directory creates the profile when it does not exist yet.
		chrome = exec.Command("open", "-na", "Google Chrome", "--args", "--profile-directory="+ap.Dir, guideURL, "chrome://extensions/")
	case "linux":
		chrome = exec.Command("google-chrome", "--profile-directory="+ap.Dir, guideURL, "chrome://extensions/")
	default:
		return fmt.Errorf("opening Chrome is not implemented for %s", runtime.GOOS)
	}
	if err := chrome.Start(); err != nil {
		return err
	}
	_ = chrome.Process.Release()
	return revealFile(filepath.Join(p.ExtensionDir, "manifest.json"))
}

func shipyardGuideHTML(p shipyardPaths, profileName string) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return r.Replace(s)
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Set up the agent's browser</title>
<style>body{font:15px/1.5 -apple-system,system-ui,sans-serif;max-width:720px;margin:40px auto;padding:0 20px;color:#222}
code{background:#f2f2f2;padding:2px 6px;border-radius:4px;font-size:13px;word-break:break-all}li{margin:10px 0}.note{color:#666;font-size:13px}</style></head>
<body><h1>Set up the agent's browser</h1>
<p>This window is Chrome's <b>` + esc(profileName) + `</b> profile. Agents use only the profiles you turned on in Shipyard → Settings → Plugins → Chrome, so here they can use every site you are signed in to.</p>
<ol>
<li>Open the <b>chrome://extensions</b> tab next to this one.</li>
<li>Turn on <b>Developer mode</b> (top right).</li>
<li>Click <b>Load unpacked</b> and choose this folder (Finder has it open, selected):<br><code>` + esc(p.ExtensionDir) + `</code></li>
<li>Check that <b>` + esc(shippedExtensionName()) + `</b> shows id <code>` + esc(p.ExtensionID) + `</code>.</li>
</ol>
<p>That's all: Shipyard → Settings → Plugins shows the plugin as set up within a few seconds.</p>
<p><b>Loaded it before?</b> Then only click the reload button (⟳) on <b>` + esc(shippedExtensionName()) + `</b> in chrome://extensions, so Chrome runs the updated copy. Later updates reload it by themselves.</p>
<p class="note">Load it only in profiles you turned on for agents. If Chrome later asks to disable developer-mode extensions, keep it on for this profile.</p>
</body></html>
`
}

func newShipyardCommand() *cobra.Command {
	var pluginDir string
	cmd := &cobra.Command{
		Use:   "shipyard",
		Short: "Set up and check Shipyard's chrome plugin (run from the plugin)",
	}
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Write the extension, register the native host, and open the agent's Chrome profile with a guide",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := resolveShipyardPaths(pluginDir)
			if err != nil {
				return err
			}
			if err := writeEmbeddedExtension(p.ExtensionDir); err != nil {
				return fmt.Errorf("writing the extension: %w", err)
			}
			if err := writeShipyardNativeManifest(p); err != nil {
				return fmt.Errorf("registering the native host: %w", err)
			}
			if err := writeWebStoreExternalExtension(p); err != nil {
				return fmt.Errorf("asking Chrome to add the extension: %w", err)
			}
			reloadErr := reloadShipyardExtension(p, 15*time.Second)
			if reloadErr != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), reloadErr)
			}
			// Open the steps in each allowed profile that lacks the
			// extension (and in the agents' own profile when only its copy
			// could not reload itself).
			opened := 0
			for _, ap := range checkShipyard(p).Profiles {
				if ap.Installed && !(reloadErr != nil && ap.Dir == shipyardProfile) {
					continue
				}
				if err := openShipyardGuide(p, ap); err != nil {
					return err
				}
				opened++
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Extension %s in %s\nNative host %s\nOpened the steps in %d profile(s)\n", p.ExtensionID, p.ExtensionDir, p.Manifest, opened)
			return nil
		},
	}
	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Report whether the chrome plugin is set up",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := resolveShipyardPaths(pluginDir)
			if err != nil {
				return err
			}
			st := checkShipyard(p)
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(st)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ready: %v\n", st.Ready)
			if st.Problem != "" {
				fmt.Fprintln(cmd.OutOrStdout(), st.Problem)
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	settings := &cobra.Command{
		Use:   "settings",
		Short: "List the plugin's settings for Shipyard: which Chrome profiles agents may use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := resolveShipyardPaths(pluginDir)
			if err != nil {
				return err
			}
			list := shipyardSettingsList(p)
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"settings": list})
			}
			for _, s := range list {
				fmt.Fprintf(cmd.OutOrStdout(), "%-5v %s  %s\n", s.Value, s.Label, s.Description)
			}
			return nil
		},
	}
	settings.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.PersistentFlags().StringVar(&pluginDir, "plugin-dir", "", "the plugin's folder (default $BERTH_PLUGIN_DIR)")
	writeExtension := &cobra.Command{
		Use:    "write-extension <dir>",
		Short:  "Write the extension exactly as the plugin ships it (for a Chrome Web Store upload)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := writeEmbeddedExtension(args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), shippedVersionName())
			return nil
		},
	}
	cmd.AddCommand(setup, status, settings, writeExtension)
	return cmd
}

// ambiguousBrowserError is returned when no browser or profile was pinned and
// more than one extension is connected: picking one could drive the wrong
// profile (a bank's, say), so the caller must say which.
func ambiguousBrowserError(connected []connectedProfileInfo) error {
	names := make([]string, 0, len(connected))
	for _, c := range connected {
		label := c.Target
		if label == "" {
			label = c.Directory
		}
		if c.DisplayName != "" {
			label += " (" + c.DisplayName + ")"
		}
		names = append(names, label)
	}
	sort.Strings(names)
	return fmt.Errorf("%d browser profiles are connected (%s); pass --browser and --profile to pick one", len(connected), strings.Join(names, ", "))
}
