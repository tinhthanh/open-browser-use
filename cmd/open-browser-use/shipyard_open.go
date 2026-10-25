package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// A box runs agents with nobody at its screen. When the profile an agent
// works in is not open, Chrome has no extension for the MCP to reach, and
// "open Chrome on the desired profile" is advice no one there can follow.
// So before a browser call the MCP opens that profile itself and waits for
// its extension to connect.

// launchChromeProfile opens Chrome on a profile (a variable for tests).
var launchChromeProfile = func(dir string) error {
	var chrome *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		// -n: a running Chrome on other profiles still opens this one.
		chrome = exec.Command("open", "-na", "Google Chrome", "--args", "--no-first-run", "--no-default-browser-check", "--profile-directory="+dir)
	case "linux":
		chrome = exec.Command("google-chrome", "--no-first-run", "--no-default-browser-check", "--profile-directory="+dir)
	default:
		return fmt.Errorf("opening Chrome is not implemented for %s", runtime.GOOS)
	}
	if err := chrome.Start(); err != nil {
		return err
	}
	_ = chrome.Process.Release()
	return nil
}

// profileOpenWait is how long the MCP waits for an opened profile's
// extension to connect; profileOpenPoll how often it looks.
var (
	profileOpenWait = 20 * time.Second
	profileOpenPoll = 250 * time.Millisecond
)

// profileConnected says whether the plugin's extension is connected in dir.
func profileConnected(p shipyardPaths, dir string) bool {
	for _, c := range connectedShipyardExtensions(p) {
		if c.Directory == dir {
			return true
		}
	}
	return false
}

// openShipyardProfile makes sure the profile selectShipyardProfile picked is
// open in Chrome with the extension connected, opening it when it is not.
func (server *mcpServer) openShipyardProfile() error {
	dir, ok := strings.CutPrefix(server.runner.options.profile, profileDirSelectorPrefix)
	if !ok {
		return nil
	}
	p, err := resolveShipyardPaths("")
	if err != nil {
		return err
	}
	if profileConnected(p, dir) {
		return nil
	}
	var ap agentProfile
	for _, a := range allowedAgentProfiles(p) {
		if a.Dir == dir {
			ap = a
		}
	}
	if ap.Dir == "" {
		return nil // selectShipyardProfile already refused it
	}
	if !ap.Installed {
		return fmt.Errorf("the agent's extension is not in the %q Chrome profile: finish the chrome plugin's setup in Shipyard → Settings → Plugins", ap.Name)
	}
	if ap.Off {
		return fmt.Errorf("the agent's extension is turned off in the %q Chrome profile: ask the user to turn it on in that profile's chrome://extensions", ap.Name)
	}
	if err := launchChromeProfile(dir); err != nil {
		return fmt.Errorf("could not open the %q Chrome profile: %w", ap.Name, err)
	}
	deadline := time.Now().Add(profileOpenWait)
	for time.Now().Before(deadline) {
		time.Sleep(profileOpenPoll)
		if profileConnected(p, dir) {
			return nil
		}
	}
	return fmt.Errorf("opened the %q Chrome profile, but its extension did not connect within %s: if the screen is locked, ask the user to unlock it, then retry", ap.Name, profileOpenWait)
}
