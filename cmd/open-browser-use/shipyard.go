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
func writeEmbeddedExtension(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return fs.WalkDir(chromeextension.Files, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || name == "embed.go" {
			return err
		}
		b, err := chromeextension.Files.ReadFile(name)
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
			want[name], _ = chromeextension.Files.ReadFile(name)
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
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.Manifest), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p.Manifest, append(payload, '\n'), 0o600)
}

type shipyardStatus struct {
	Ready              bool   `json:"ready"`
	NativeHost         bool   `json:"nativeHost"`
	Profile            bool   `json:"profile"`
	ExtensionInstalled bool   `json:"extensionInstalled"`
	ExtensionIntact    bool   `json:"extensionIntact"`
	Connected          bool   `json:"connected"`
	ProfileName        string `json:"profileName"`
	ExtensionID        string `json:"extensionId"`
	ExtensionDir       string `json:"extensionDir"`
	Problem            string `json:"problem,omitempty"`
}

func checkShipyard(p shipyardPaths) shipyardStatus {
	st := shipyardStatus{ProfileName: shipyardProfile, ExtensionID: p.ExtensionID, ExtensionDir: p.ExtensionDir}
	if b, err := os.ReadFile(p.Manifest); err == nil {
		var m struct {
			Path           string   `json:"path"`
			AllowedOrigins []string `json:"allowed_origins"`
		}
		if json.Unmarshal(b, &m) == nil && m.Path == p.Binary && len(m.AllowedOrigins) == 1 && m.AllowedOrigins[0] == "chrome-extension://"+p.ExtensionID+"/" {
			st.NativeHost = true
		}
	}
	if info, err := os.Stat(p.ProfileDir); err == nil && info.IsDir() {
		st.Profile = true
	}
	if d, ok := detectUnpackedExtensionInProfile(p.ProfileDir, p.ExtensionID); ok && d.Version != "" {
		st.ExtensionInstalled = true
	}
	var why string
	st.ExtensionIntact, why = extensionIntact(p.ExtensionDir)
	for _, c := range probeConnectedProfiles(host.DefaultSocketDir) {
		if c.ExtensionID == p.ExtensionID && c.Directory == shipyardProfile {
			st.Connected = true
		}
	}
	st.Ready = st.NativeHost && st.ExtensionInstalled && st.ExtensionIntact
	switch {
	case !st.ExtensionIntact:
		st.Problem = "the extension folder is not the one this plugin ships (" + why + "); run setup again"
	case !st.NativeHost:
		st.Problem = "the native host is not registered for this extension; run setup again"
	case !st.Profile:
		st.Problem = "Chrome has no \"" + shipyardProfile + "\" profile yet"
	case !st.ExtensionInstalled:
		st.Problem = "load the extension in the \"" + shipyardProfile + "\" profile (chrome://extensions, Developer mode, Load unpacked)"
	}
	return st
}

func openShipyardGuide(p shipyardPaths) error {
	guide := filepath.Join(p.PluginDir, "setup.html")
	if err := os.WriteFile(guide, []byte(shipyardGuideHTML(p)), 0o644); err != nil {
		return err
	}
	guideURL := "file://" + guide
	var chrome *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// --profile-directory creates the profile when it does not exist yet.
		chrome = exec.Command("open", "-na", "Google Chrome", "--args", "--profile-directory="+shipyardProfile, guideURL, "chrome://extensions/")
	case "linux":
		chrome = exec.Command("google-chrome", "--profile-directory="+shipyardProfile, guideURL, "chrome://extensions/")
	default:
		return fmt.Errorf("opening Chrome is not implemented for %s", runtime.GOOS)
	}
	if err := chrome.Start(); err != nil {
		return err
	}
	_ = chrome.Process.Release()
	return revealFile(filepath.Join(p.ExtensionDir, "manifest.json"))
}

func shipyardGuideHTML(p shipyardPaths) string {
	esc := func(s string) string {
		r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
		return r.Replace(s)
	}
	return `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Set up the agent's browser</title>
<style>body{font:15px/1.5 -apple-system,system-ui,sans-serif;max-width:720px;margin:40px auto;padding:0 20px;color:#222}
code{background:#f2f2f2;padding:2px 6px;border-radius:4px;font-size:13px;word-break:break-all}li{margin:10px 0}.note{color:#666;font-size:13px}</style></head>
<body><h1>Set up the agent's browser</h1>
<p>This window is Chrome's <b>` + esc(shipyardProfile) + `</b> profile. Agents use only this profile, never your other ones, so sign in here only to sites you want agents to use.</p>
<ol>
<li>Open the <b>chrome://extensions</b> tab next to this one.</li>
<li>Turn on <b>Developer mode</b> (top right).</li>
<li>Click <b>Load unpacked</b> and choose this folder (Finder has it open, selected):<br><code>` + esc(p.ExtensionDir) + `</code></li>
<li>Check that <b>Open Browser Use</b> shows id <code>` + esc(p.ExtensionID) + `</code>.</li>
</ol>
<p>That's all: Shipyard → Settings → Plugins shows the plugin as set up within a few seconds.</p>
<p class="note">Do not load this extension in other profiles. If Chrome later asks to disable developer-mode extensions, keep it on for this profile.</p>
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
			st := checkShipyard(p)
			if !st.ExtensionInstalled {
				if err := openShipyardGuide(p); err != nil {
					return err
				}
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Extension %s in %s\nNative host %s\nProfile %q\n", p.ExtensionID, p.ExtensionDir, p.Manifest, shipyardProfile)
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
	cmd.PersistentFlags().StringVar(&pluginDir, "plugin-dir", "", "the plugin's folder (default $BERTH_PLUGIN_DIR)")
	cmd.AddCommand(setup, status)
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
