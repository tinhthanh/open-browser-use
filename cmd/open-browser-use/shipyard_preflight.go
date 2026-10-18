package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// What has to be true on a Mac before the extension can work at all, and
// that setup can't do by loading files: Chrome is there, and the login
// keychain (where Chrome keeps the key to its cookies) is the default one.
// Found the hard way on fresh boxes: no Chrome, and a Chrome that stalled
// every page on "Keychain Not Found".

// chromeApps are where Google Chrome can be installed on a Mac.
func chromeApps() []string {
	home, _ := os.UserHomeDir()
	return []string{"/Applications/Google Chrome.app", filepath.Join(home, "Applications", "Google Chrome.app")}
}

// chromeInstalled says whether Google Chrome is on this computer (a
// variable for tests).
var chromeInstalled = func() bool {
	switch runtime.GOOS {
	case "darwin":
		for _, app := range chromeApps() {
			if info, err := os.Stat(filepath.Join(app, "Contents", "MacOS", "Google Chrome")); err == nil && !info.IsDir() {
				return true
			}
		}
		return false
	case "linux":
		for _, name := range []string{"google-chrome", "google-chrome-stable"} {
			if _, err := exec.LookPath(name); err == nil {
				return true
			}
		}
		return false
	}
	return true
}

// keychainProblem says what is wrong with the user's keychains for Chrome,
// or "" (a variable for tests). Without a default keychain Chrome asks for
// one in a dialog nobody sees on a box, and no page loads until it is
// answered.
var keychainProblem = func() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "security", "default-keychain", "-d", "user").Run(); err != nil {
		return "this Mac's user has no default keychain, so Chrome can't keep its data and stalls on \"Keychain Not Found\": " +
			"open Keychain Access and make \"login\" the default keychain (or run: security default-keychain -d user -s ~/Library/Keychains/login.keychain-db)"
	}
	return ""
}

// chromeDisplayName is what Chrome calls a profile in its own windows, from
// Local State: a profile made by --profile-directory is often "Your Chrome"
// there, whatever its folder is called.
func chromeDisplayName(p shipyardPaths, dir string) string {
	b, err := os.ReadFile(filepath.Join(p.ChromeRoot, "Local State"))
	if err != nil {
		return ""
	}
	var ls struct {
		Profile struct {
			InfoCache map[string]struct {
				Name string `json:"name"`
			} `json:"info_cache"`
		} `json:"profile"`
	}
	if json.Unmarshal(b, &ls) != nil {
		return ""
	}
	return ls.Profile.InfoCache[dir].Name
}

// profileWords names a profile for the person: its name, and what Chrome
// shows for it when that differs.
func profileWords(p shipyardPaths, ap agentProfile) string {
	words := "the \"" + ap.Name + "\" profile"
	if shown := chromeDisplayName(p, ap.Dir); shown != "" && shown != ap.Name {
		words += " (Chrome shows it as \"" + shown + "\")"
	}
	return words
}

// The official installer, and the team that signs Google Chrome.
const (
	chromeDMG    = "https://dl.google.com/chrome/mac/universal/stable/GGRO/googlechrome.dmg"
	chromeTeamID = "EQHXZ8M8AV"
)

// installChrome puts Google Chrome on this Mac from Google's own installer:
// in /Applications when the user can write there, else ~/Applications. It
// copies the app only once its signature is Google's and Gatekeeper accepts
// it.
func installChrome(out io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("install Google Chrome from https://www.google.com/chrome/")
	}
	tmp, err := os.MkdirTemp("", "obu-chrome-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dmg := filepath.Join(tmp, "googlechrome.dmg")
	fmt.Fprintln(out, "Downloading Google Chrome from dl.google.com…")
	if err := download(chromeDMG, dmg); err != nil {
		return fmt.Errorf("downloading Chrome: %w", err)
	}
	mount := filepath.Join(tmp, "mnt")
	if b, err := exec.Command("hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mount, dmg).CombinedOutput(); err != nil {
		return fmt.Errorf("opening the installer: %v: %s", err, b)
	}
	defer exec.Command("hdiutil", "detach", "-quiet", mount).Run()
	app := filepath.Join(mount, "Google Chrome.app")
	if err := verifyChrome(app); err != nil {
		return err
	}
	dest := "/Applications"
	if f, err := os.CreateTemp(dest, ".obu-write-"); err == nil {
		f.Close()
		os.Remove(f.Name())
	} else {
		home, _ := os.UserHomeDir()
		dest = filepath.Join(home, "Applications")
		os.MkdirAll(dest, 0o755)
	}
	fmt.Fprintln(out, "Installing Google Chrome in "+dest+"…")
	if b, err := exec.Command("ditto", app, filepath.Join(dest, "Google Chrome.app")).CombinedOutput(); err != nil {
		return fmt.Errorf("copying Chrome: %v: %s", err, b)
	}
	return nil
}

// verifyChrome accepts the app only when Google's team signed it and
// Gatekeeper takes it.
func verifyChrome(app string) error {
	b, _ := exec.Command("codesign", "-dv", "--verbose=2", app).CombinedOutput()
	if !signedBy(string(b), chromeTeamID) {
		return errors.New("the downloaded Chrome isn't signed by Google; not installing it")
	}
	if b, err := exec.Command("spctl", "--assess", "--type", "execute", app).CombinedOutput(); err != nil {
		return fmt.Errorf("Gatekeeper refuses the downloaded Chrome: %s", strings.TrimSpace(string(b)))
	}
	return nil
}

// signedBy reads codesign -dv's output for its team.
func signedBy(codesign, team string) bool {
	for _, line := range strings.Split(codesign, "\n") {
		if strings.TrimSpace(line) == "TeamIdentifier="+team {
			return true
		}
	}
	return false
}

func download(url, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s", resp.Status)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
