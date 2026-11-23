package cmd

import (
	"context"
	"text/tabwriter"
	"time"

	"encoding/json"
	"errors"
	"fmt"
	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
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
//   sudo cerver addons set <level>  off | malware | adult | kids, here, now
//
// Any computer on your network (agents use these; the app does the same):
//   cerver addons list                              every computer's add-ons
//   cerver addons <computer> safety <level>         off | malware | adult | kids
//   cerver addons <computer> focus <when> [sites…]  off | work_hours | hour | always
//                                                   sites: facebook instagram youtube
//                                                   x tiktok reddit netflix linkedin
//                                                   or any domain; none = keep the list
// The computer's Cerver network service applies it within about 20 seconds.
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
	case "list":
		return listRemoteAddons(len(args) > 0 && args[0] == "--json")
	case "import-block":
		if len(args) != 1 {
			return errors.New("usage: cerver addons import-block <computer>   (reads ~/.config/block)")
		}
		return importBlock(args[0])
	}
	if len(args) >= 2 && (args[0] == "safety" || args[0] == "focus") {
		return setRemoteAddons(sub, args[0], args[1], args[2:])
	}
	return fmt.Errorf("unknown: cerver addons %s (status | list | <computer> safety <level> | <computer> focus <when> [sites…])", sub)
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

// ── Any computer on the network, through the gateway

type remoteNode struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	OS     string `json:"os"`
	Online bool   `json:"online"`
	Addons *struct {
		Level string        `json:"level"`
		Focus focusSettings `json:"focus"`
	} `json:"addons"`
	Applied json.RawMessage `json:"addons_applied"`
}

func remoteNodes(ctx context.Context) ([]remoteNode, *gateway.Client, error) {
	gw, err := netGateway(ctx)
	if err != nil {
		return nil, nil, err
	}
	var out struct {
		Nodes []remoteNode `json:"nodes"`
	}
	if err := gw.Do(ctx, "GET", "/v2/net", nil, &out); err != nil {
		return nil, nil, err
	}
	return out.Nodes, gw, nil
}

func listRemoteAddons(asJSON bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, _, err := remoteNodes(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		b, _ := json.Marshal(nodes)
		fmt.Println(string(b))
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "COMPUTER\tSAFETY\tFOCUS\tSITES\tAPPLIED")
	for _, n := range nodes {
		lvl, pol, sites := "off", "off", "-"
		if n.Addons != nil {
			lvl = n.Addons.Level
			var ps, ss []string
			for _, r := range n.Addons.Focus.rules() {
				name := r.Name
				if name != "" {
					name += ":"
				}
				ps = append(ps, name+r.Policy)
				ss = append(ss, strings.Join(r.Sites, ","))
			}
			pol, sites = strings.Join(ps, " "), strings.Join(ss, " | ")
			if strings.Trim(sites, " |") == "" {
				sites = "-"
			}
		}
		applied := "needs a Cerver update"
		if len(n.Applied) > 0 && string(n.Applied) != "null" {
			applied = "yes"
		}
		if !n.Online {
			applied = "offline"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", n.Name, lvl, pol, sites, applied)
	}
	return w.Flush()
}

func setRemoteAddons(computer, what, value string, sites []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, gw, err := remoteNodes(ctx)
	if err != nil {
		return err
	}
	var n *remoteNode
	want := strings.ToLower(computer)
	for i := range nodes {
		if strings.ToLower(nodes[i].Name) == want || nodes[i].IP == computer || wgHost(nodes[i].Name) == wgHost(computer) {
			n = &nodes[i]
		}
	}
	if n == nil { // "mac-mini" finds "eyals-mac-mini" when only one matches
		var hit []int
		for i := range nodes {
			if strings.Contains(wgHost(nodes[i].Name), wgHost(computer)) {
				hit = append(hit, i)
			}
		}
		if len(hit) == 1 {
			n = &nodes[hit[0]]
		}
	}
	if n == nil {
		var names []string
		for _, x := range nodes {
			names = append(names, x.Name)
		}
		return fmt.Errorf("no computer %q on your network (have: %s)", computer, strings.Join(names, ", "))
	}
	body := map[string]any{}
	switch what {
	case "safety":
		ok := false
		for _, l := range addonLevels {
			ok = ok || l.ID == value
		}
		if !ok {
			return errors.New("safety: off, malware, adult or kids")
		}
		body["level"] = value
	case "focus":
		pol := value
		if pol == "hour" {
			pol = "until"
		}
		if pol != "off" && pol != "work_hours" && pol != "until" && pol != "always" {
			return errors.New("focus: off, work_hours, hour or always")
		}
		var cur focusSettings
		if n.Addons != nil {
			cur = n.Addons.Focus
		}
		rules := cur.rules()
		// Which rule: the one named by a leading "rule=<name>" site, else the first.
		idx := 0
		if len(sites) > 0 && strings.HasPrefix(sites[0], "rule=") {
			name := strings.TrimPrefix(sites[0], "rule=")
			sites = sites[1:]
			idx = -1
			for i, r := range rules {
				if strings.EqualFold(r.Name, name) {
					idx = i
				}
			}
			if idx < 0 {
				rules = append(rules, focusSettings{Name: name})
				idx = len(rules) - 1
			}
		}
		r := rules[idx]
		r.Policy = pol
		if len(sites) > 0 {
			r.Sites = sites
		}
		r.Until = 0
		if pol == "until" {
			r.Until = time.Now().Add(time.Hour).Unix()
		}
		r.Hours, r.Rules = nil, nil
		rules[idx] = r
		body["focus"] = map[string]any{"rules": rules}
	}
	var out struct {
		Say string `json:"say"`
	}
	if err := gw.Do(ctx, "PUT", "/v2/net/nodes/"+n.IP+"/addons", body, &out); err != nil {
		return err
	}
	fmt.Printf("%s: %s set to %s. %s\n", n.Name, what, value, out.Say)
	return nil
}

func wgHost(s string) string { return wgnet.HostName(s) }

// importBlock moves the rules of the "block" tool (~/.config/block: <name>.list
// files and a schedule of "HH:MM <list|off>" lines, every day) into Cerver
// Focus rules for a computer: one rule per list, each with the schedule that
// switches it on and off.
func importBlock(computer string) error {
	dir := filepath.Join(os.Getenv("HOME"), ".config", "block")
	lists := map[string][]string{}
	var names []string
	files, _ := filepath.Glob(filepath.Join(dir, "*.list"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(f), ".list")
		for _, line := range strings.Split(string(b), "\n") {
			if d := strings.TrimSpace(line); d != "" && !strings.HasPrefix(d, "#") {
				lists[name] = append(lists[name], d)
			}
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return fmt.Errorf("no lists in %s", dir)
	}
	sched := map[string][]schedEntry{}
	if b, err := os.ReadFile(filepath.Join(dir, "schedule")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 || strings.HasPrefix(f[0], "#") || len(f[0]) != 5 || f[0][2] != ':' {
				continue
			}
			// At this time, list f[1] turns on and every other list turns off.
			for _, n := range names {
				sched[n] = append(sched[n], schedEntry{At: f[0], Block: n == f[1]})
			}
		}
	}
	var rules []focusSettings
	for _, n := range names {
		r := focusSettings{Name: n, Sites: lists[n], Policy: "off"}
		has := false
		for _, e := range sched[n] {
			has = has || e.Block
		}
		if has {
			r.Policy, r.Schedule = "schedule", sched[n]
		}
		rules = append(rules, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, gw, err := remoteNodes(ctx)
	if err != nil {
		return err
	}
	var target *remoteNode
	for i := range nodes {
		if strings.EqualFold(nodes[i].Name, computer) || strings.Contains(wgHost(nodes[i].Name), wgHost(computer)) {
			target = &nodes[i]
		}
	}
	if target == nil {
		return fmt.Errorf("no computer %q on your network", computer)
	}
	// Keep the computer's other rules (e.g. one made in the app).
	if target.Addons != nil {
		for _, r := range target.Addons.Focus.rules() {
			dup := false
			for _, n := range names {
				dup = dup || strings.EqualFold(r.Name, n)
			}
			if !dup && len(r.Sites) > 0 {
				if r.Name == "" {
					r.Name = "Focus"
				}
				r.Hours, r.Rules = nil, nil
				rules = append(rules, r)
			}
		}
	}
	var out struct {
		Say string `json:"say"`
	}
	if err := gw.Do(ctx, "PUT", "/v2/net/nodes/"+target.IP+"/addons", map[string]any{"focus": map[string]any{"rules": rules}}, &out); err != nil {
		return err
	}
	for _, r := range rules {
		fmt.Printf("  %s: %s · %s\n", r.Name, r.Policy, strings.Join(r.Sites, ", "))
	}
	fmt.Printf("%s: imported %d rules from %s. %s\n", target.Name, len(rules), dir, out.Say)
	return nil
}
