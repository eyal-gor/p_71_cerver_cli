package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// `cerver addons` turns on safety add-ons for a whole computer: the internet
// on it (every app, not just agents) goes through a filtering DNS.
//
//   cerver addons                   what's on here (--json for the app)
//   sudo cerver addons set <level>  off | malware | adult | kids
//
// Levels, each one includes the one before:
//   malware  blocks malware and phishing sites (Cloudflare 1.1.1.2)
//   adult    also blocks adult sites (Cloudflare for Families 1.1.1.3)
//   kids     also forces SafeSearch on Google, Bing and YouTube and blocks
//            proxies and VPNs used to get around it (CleanBrowsing Family)
//
// On a Mac it sets the DNS servers of every network service (Wi-Fi,
// Ethernet, …) with networksetup; "off" puts back automatic DNS from the
// router. Cerver's own computer names (<name>.cerver) live in /etc/hosts, so
// they keep working. The choice is saved in ~/.cerver/addons.json.

type addonLevel struct {
	ID, Name, What string
	DNS            []string
}

var addonLevels = []addonLevel{
	{"off", "Off", "Your router's DNS, nothing filtered.", nil},
	{"malware", "Block malware", "Blocks malware and phishing sites.", []string{"1.1.1.2", "1.0.0.2", "2606:4700:4700::1112", "2606:4700:4700::1002"}},
	{"adult", "Block adult sites", "Blocks adult sites, malware and phishing.", []string{"1.1.1.3", "1.0.0.3", "2606:4700:4700::1113", "2606:4700:4700::1003"}},
	{"kids", "Kids safety", "Blocks adult sites, malware and the proxies used to get around a filter; forces SafeSearch on Google, Bing and YouTube.", []string{"185.228.168.168", "185.228.169.168", "2a0d:2a00:1::", "2a0d:2a00:2::"}},
}

type AddonsStatus struct {
	Supported bool        `json:"supported"`
	Why       string      `json:"why,omitempty"`
	Level     string      `json:"level"`           // what the DNS actually is now
	Saved     string      `json:"saved,omitempty"` // what was chosen
	DNS       []string    `json:"dns"`
	Services  []string    `json:"services"`
	Focus     FocusStatus `json:"focus"`
}

func Addons(args []string) error {
	sub := "status"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "status":
		st := getAddons()
		if len(args) > 0 && args[0] == "--json" {
			b, _ := json.Marshal(st)
			fmt.Println(string(b))
			return nil
		}
		if !st.Supported {
			fmt.Println("Add-ons:", st.Why)
			return nil
		}
		fmt.Printf("Add-ons: %s (DNS %s)\n", levelName(st.Level), strings.Join(st.DNS, ", "))
		return nil
	case "set":
		if len(args) != 1 {
			return errors.New("usage: sudo cerver addons set off|malware|adult|kids")
		}
		return setAddons(args[0])
	}
	return fmt.Errorf("unknown: cerver addons %s (status | set)", sub)
}

func levelName(id string) string {
	for _, l := range addonLevels {
		if l.ID == id {
			return l.Name
		}
	}
	return "Custom DNS"
}

func addonsFile() string {
	home := os.Getenv("HOME")
	if u := os.Getenv("SUDO_USER"); u != "" && os.Geteuid() == 0 {
		home = filepath.Join("/Users", u)
	}
	return filepath.Join(home, ".cerver", "addons.json")
}

// networkServices: the Mac's enabled network services (Wi-Fi, Ethernet, …).
func networkServices() []string {
	out, err := exec.Command("/usr/sbin/networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil
	}
	var s []string
	for i, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if i == 0 || line == "" || strings.HasPrefix(line, "*") { // header; * = disabled
			continue
		}
		s = append(s, line)
	}
	return s
}

func dnsOf(service string) []string {
	out, err := exec.Command("/usr/sbin/networksetup", "-getdnsservers", service).Output()
	if err != nil || strings.Contains(string(out), "aren't any") {
		return nil
	}
	return strings.Fields(string(out))
}

func getAddons() AddonsStatus {
	st := AddonsStatus{Level: "off", DNS: []string{}, Services: []string{}}
	if runtime.GOOS != "darwin" {
		st.Why = "add-ons work on Macs for now"
		return st
	}
	st.Supported = true
	st.Services = networkServices()
	// The level is what the DNS really is: the first service that has one set.
	for _, s := range st.Services {
		if d := dnsOf(s); len(d) > 0 {
			st.DNS = d
			st.Level = "custom"
			for _, l := range addonLevels {
				if len(l.DNS) > 0 && d[0] == l.DNS[0] {
					st.Level = l.ID
				}
			}
			break
		}
	}
	if b, err := os.ReadFile(addonsFile()); err == nil {
		var saved struct{ Level string }
		if json.Unmarshal(b, &saved) == nil {
			st.Saved = saved.Level
		}
	}
	st.Focus = focusStatus()
	return st
}

func setAddons(id string) error {
	var lvl *addonLevel
	for i := range addonLevels {
		if addonLevels[i].ID == id {
			lvl = &addonLevels[i]
		}
	}
	if lvl == nil {
		return fmt.Errorf("unknown level %q: off, malware, adult or kids", id)
	}
	if runtime.GOOS != "darwin" {
		return errors.New("add-ons work on Macs for now")
	}
	if os.Geteuid() != 0 {
		return errors.New("changing the DNS needs admin: run it with sudo (the Cerver app asks for your password)")
	}
	services := networkServices()
	if len(services) == 0 {
		return errors.New("found no network services on this Mac")
	}
	dns := lvl.DNS
	if len(dns) == 0 {
		dns = []string{"Empty"} // automatic DNS from the router
	}
	for _, s := range services {
		args := append([]string{"-setdnsservers", s}, dns...)
		if out, err := exec.Command("/usr/sbin/networksetup", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %s", s, strings.TrimSpace(string(out)))
		}
	}
	// New answers right away, not after the old ones expire.
	_ = exec.Command("/usr/bin/dscacheutil", "-flushcache").Run()
	_ = exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()

	f := addonsFile()
	_ = os.MkdirAll(filepath.Dir(f), 0o755)
	// Keep the rest of the file (the Focus settings).
	all := map[string]any{}
	if old, err := os.ReadFile(f); err == nil {
		_ = json.Unmarshal(old, &all)
	}
	all["level"] = id
	b, _ := json.Marshal(all)
	if err := os.WriteFile(f, b, 0o644); err == nil {
		if u := os.Getenv("SUDO_USER"); u != "" {
			_ = exec.Command("/usr/sbin/chown", u, f).Run()
		}
	}
	fmt.Printf("%s: on for %s\n", lvl.Name, strings.Join(services, ", "))
	return nil
}
