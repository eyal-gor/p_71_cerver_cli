package cmd

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
)

// Exit nodes: one device (the Mac mini at home) lets your other devices send
// all their internet through it, so on café Wi-Fi the café sees one encrypted
// connection and websites see your home's address.
//
// The exit: IP forwarding on, and macOS's own firewall (pf) translating the
// network's addresses to its own, in an anchor of the default rules.
//
// A device using it: WireGuard sends every address to the exit peer; two
// half routes (0/1, 128/1) point at the tunnel without touching the real
// default route; Cerver's own connections (gateway, STUN, peers' direct
// addresses) keep host routes to the real router. DNS moves to a public
// resolver (reached through the tunnel), and IPv6 is off meanwhile (the
// tunnel is IPv4), so neither goes around it. When the exit stops answering
// for 3 minutes the device goes back to its normal connection by itself.
//
// What was changed is saved in exitStateFile, so a crash or update undoes it
// at the next start.

// exitDNS: the resolver while using an exit (reached through the tunnel).
var exitDNS = []string{"1.1.1.1", "1.0.0.1"}

const (
	exitStateFile = "/var/db/cerver-exit.json"
	exitAnchor    = "com.apple/250.CerverExit"
)

type exitState struct {
	// Using an exit.
	Via    string   `json:"via,omitempty"`
	Routes []string `json:"routes,omitempty"` // host routes to the real router
	Gw     string   `json:"gw,omitempty"`
	DNSSet bool     `json:"dns_set,omitempty"`
	V6Off  []string `json:"v6_off,omitempty"` // network services we turned IPv6 off on
	Halves bool     `json:"halves,omitempty"`
	// Being an exit.
	Offering bool   `json:"offering,omitempty"`
	NatIface string `json:"nat_iface,omitempty"`
	PfToken  string `json:"pf_token,omitempty"`
	FwdWas   string `json:"fwd_was,omitempty"`
}

type exitCtl struct {
	ifname  string
	gateway string // Cerver gateway host
	st      exitState
	why     string
	misses  int
	resolve time.Time
	hosts   []string // gateway + STUN addresses, resolved
}

var (
	exitMu     sync.Mutex
	exitReport = map[string]any{}
)

// exitApplied: what's in effect, for the owner's app (part of addons_applied).
func exitApplied() map[string]any {
	exitMu.Lock()
	defer exitMu.Unlock()
	out := map[string]any{}
	for k, v := range exitReport {
		out[k] = v
	}
	return out
}

func newExitCtl(ifname, gatewayURL string) *exitCtl {
	e := &exitCtl{ifname: ifname}
	if u, err := url.Parse(gatewayURL); err == nil {
		e.gateway = u.Hostname()
	}
	// Undo what a previous run left (crash, update, restart).
	if b, err := os.ReadFile(exitStateFile); err == nil {
		var old exitState
		if json.Unmarshal(b, &old) == nil {
			e.st = old
			e.stopUsing("restarted")
			e.stopOffering()
		}
	}
	return e
}

func (e *exitCtl) save() {
	b, _ := json.Marshal(e.st)
	_ = os.WriteFile(exitStateFile, b, 0o600)
	exitMu.Lock()
	exitReport = map[string]any{"offering": e.st.Offering, "via": e.st.Via, "active": e.st.Halves, "why": e.why}
	exitMu.Unlock()
}

// update brings the device in line with what the owner chose. Called every
// few seconds.
func (e *exitCtl) update(node *wgnet.Node) {
	ans := node.Answer
	if ans == nil {
		return
	}
	if runtime.GOOS != "darwin" {
		if ans.Exit.Offer || ans.Exit.Via != "" {
			e.why = "exit nodes work on Macs for now"
			e.save()
		}
		return
	}
	// Being an exit.
	if ans.Exit.Offer {
		e.offer()
	} else if e.st.Offering {
		e.stopOffering()
	}
	// Using one.
	via := ans.Exit.Via
	if via == "" {
		if e.st.Via != "" {
			e.stopUsing("")
			node.SetExitPeer("")
		}
		return
	}
	var peer *wgnet.PeerStatus
	for _, p := range node.Status() {
		if p.IP == via {
			p := p
			peer = &p
		}
	}
	if e.st.Via != "" && e.st.Via != via { // switched to another exit
		e.stopUsing("")
		node.SetExitPeer("")
	}
	// Healthy: WireGuard has the peer, and it answers a ping through the
	// tunnel (every few seconds), so a dead exit is noticed in ~10 s, not 3 min.
	healthy := peer != nil && peer.Connected && pingOK(via)
	if healthy {
		e.misses = 0
	} else {
		e.misses++
	}
	switch {
	case healthy && !e.st.Halves:
		if err := e.startUsing(node, via); err != nil {
			e.stopUsing(err.Error())
			node.SetExitPeer("")
		}
	case healthy:
		e.refreshRoutes(node) // a peer moved, the router changed
	case !healthy && e.st.Halves:
		// 3 missed checks in a row (~10 s): back to the normal connection.
		if e.misses >= 3 {
			name := via
			if peer != nil {
				name = peer.Name
			}
			e.stopUsing(name + " isn't answering; using this network directly until it's back")
			node.SetExitPeer("")
		}
	case !healthy:
		name := via
		if peer != nil {
			name = peer.Name
		}
		e.why = "waiting for " + name
		e.save()
	}
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// physicalDefault: the real router and interface (not our tunnel).
func physicalDefault() (gw, iface string) {
	out, err := exec.Command("/usr/sbin/netstat", "-rn", "-f", "inet").Output()
	if err != nil {
		return "", ""
	}
	return parseDefault(string(out))
}

func parseDefault(netstat string) (gw, iface string) {
	for _, line := range strings.Split(netstat, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "default" && !strings.HasPrefix(f[len(f)-1], "utun") {
			if a, err := netip.ParseAddr(f[1]); err == nil && a.Is4() {
				return f[1], f[len(f)-1]
			}
		}
	}
	return "", ""
}

// publicIP: an address worth a host route (not private, not ours).
func publicIP(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil || !a.Is4() {
		return false
	}
	return !(a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || netip.MustParsePrefix("100.64.0.0/10").Contains(a))
}

// mustBypass: addresses Cerver itself talks to, which keep going direct.
func (e *exitCtl) mustBypass(node *wgnet.Node) []string {
	if time.Since(e.resolve) > time.Minute || len(e.hosts) == 0 {
		var hosts []string
		names := []string{e.gateway}
		for _, s := range wgnet.STUNServers {
			if h, _, err := net.SplitHostPort(s); err == nil {
				names = append(names, h)
			}
		}
		for _, n := range names {
			if n == "" {
				continue
			}
			if ips, err := net.LookupIP(n); err == nil {
				for _, ip := range ips {
					if ip.To4() != nil {
						hosts = append(hosts, ip.String())
					}
				}
			}
		}
		if len(hosts) > 0 {
			e.hosts, e.resolve = hosts, time.Now()
		}
	}
	set := map[string]bool{}
	for _, h := range e.hosts {
		set[h] = true
	}
	for _, p := range node.Status() {
		if h, _, err := net.SplitHostPort(p.Endpoint); err == nil {
			set[h] = true
		}
	}
	var out []string
	for h := range set {
		if publicIP(h) {
			out = append(out, h)
		}
	}
	return out
}

func (e *exitCtl) refreshRoutes(node *wgnet.Node) {
	gw, _ := physicalDefault()
	if gw == "" {
		return
	}
	if gw != e.st.Gw { // another network (new Wi-Fi): routes to the new router
		for _, r := range e.st.Routes {
			run("/sbin/route", "-q", "-n", "delete", "-host", r)
		}
		e.st.Routes, e.st.Gw = nil, gw
	}
	have := map[string]bool{}
	for _, r := range e.st.Routes {
		have[r] = true
	}
	changed := false
	for _, h := range e.mustBypass(node) {
		if !have[h] {
			if _, err := run("/sbin/route", "-q", "-n", "add", "-host", h, gw); err == nil {
				e.st.Routes = append(e.st.Routes, h)
				changed = true
			}
		}
	}
	if changed {
		e.save()
	}
}

func (e *exitCtl) startUsing(node *wgnet.Node, via string) error {
	gw, _ := physicalDefault()
	if gw == "" {
		return fmt.Errorf("no internet connection to send through the exit node")
	}
	e.st.Via, e.st.Gw = via, gw
	e.save() // record before changing anything
	e.refreshRoutes(node)
	if err := node.SetExitPeer(via); err != nil {
		return err
	}
	for _, half := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
		if _, err := run("/sbin/route", "-q", "-n", "add", "-net", half, "-interface", e.ifname); err != nil {
			return err
		}
	}
	e.st.Halves = true
	e.save()
	// DNS: a public resolver, reached through the tunnel (Safety already sets one).
	if getAddons().Level == "off" {
		for _, s := range networkServices() {
			run("/usr/sbin/networksetup", append([]string{"-setdnsservers", s}, exitDNS...)...)
		}
		e.st.DNSSet = true
	}
	// IPv6 off meanwhile: the tunnel is IPv4, so IPv6 would go around it.
	for _, s := range networkServices() {
		if out, err := run("/usr/sbin/networksetup", "-getinfo", s); err == nil && regexp.MustCompile(`(?m)^IPv6: Automatic`).MatchString(out) {
			if _, err := run("/usr/sbin/networksetup", "-setv6off", s); err == nil {
				e.st.V6Off = append(e.st.V6Off, s)
			}
		}
	}
	flushDNS()
	e.why = ""
	e.save()
	fmt.Printf("Internet now goes through the exit node %s\n", via)
	return nil
}

func (e *exitCtl) stopUsing(why string) {
	if e.st.Halves {
		for _, half := range []string{"0.0.0.0/1", "128.0.0.0/1"} {
			run("/sbin/route", "-q", "-n", "delete", "-net", half)
		}
		fmt.Println("Internet goes directly again (exit node off)")
	}
	for _, r := range e.st.Routes {
		run("/sbin/route", "-q", "-n", "delete", "-host", r)
	}
	// Put DNS back unless Safety changed it meanwhile (then it's Safety's).
	if e.st.DNSSet && strings.Join(getAddons().DNS, " ") == strings.Join(exitDNS, " ") {
		for _, s := range networkServices() {
			run("/usr/sbin/networksetup", "-setdnsservers", s, "Empty")
		}
	}
	for _, s := range e.st.V6Off {
		run("/usr/sbin/networksetup", "-setv6automatic", s)
	}
	if e.st.Halves || e.st.DNSSet || len(e.st.V6Off) > 0 {
		flushDNS()
	}
	e.st.Via, e.st.Routes, e.st.Gw, e.st.DNSSet, e.st.V6Off, e.st.Halves = "", nil, "", false, nil, false
	e.why = why
	e.save()
}

func flushDNS() {
	_ = exec.Command("/usr/bin/dscacheutil", "-flushcache").Run()
	_ = exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()
}

// natRule: the network's addresses leave as this computer's own.
func natRule(iface string) string {
	return fmt.Sprintf("nat on %s inet from 100.96.0.0/16 to any -> (%s)\n", iface, iface)
}

func (e *exitCtl) offer() {
	_, iface := physicalDefault()
	if iface == "" || (e.st.Offering && iface == e.st.NatIface) {
		return
	}
	if !e.st.Offering {
		out, _ := exec.Command("/usr/sbin/sysctl", "-n", "net.inet.ip.forwarding").Output()
		e.st.FwdWas = strings.TrimSpace(string(out))
	}
	e.st.Offering = true
	e.save()
	if _, err := run("/usr/sbin/sysctl", "-w", "net.inet.ip.forwarding=1"); err != nil {
		e.why = "couldn't turn on forwarding: " + err.Error()
		e.save()
		return
	}
	// The default rules include anchors under com.apple/; make sure they're loaded.
	if out, _ := exec.Command("/sbin/pfctl", "-s", "nat").CombinedOutput(); !strings.Contains(string(out), "com.apple") {
		run("/sbin/pfctl", "-f", "/etc/pf.conf")
	}
	cmd := exec.Command("/sbin/pfctl", "-a", exitAnchor, "-f", "-")
	cmd.Stdin = strings.NewReader(natRule(iface))
	if out, err := cmd.CombinedOutput(); err != nil {
		e.why = "couldn't set up the firewall: " + strings.TrimSpace(string(out))
		e.save()
		return
	}
	if e.st.PfToken == "" {
		out, _ := exec.Command("/sbin/pfctl", "-E").CombinedOutput()
		if m := regexp.MustCompile(`Token : (\d+)`).FindStringSubmatch(string(out)); m != nil {
			e.st.PfToken = m[1]
		}
	}
	e.st.NatIface = iface
	e.why = ""
	e.save()
	fmt.Printf("Offering this computer as an exit node (through %s)\n", iface)
}

func (e *exitCtl) stopOffering() {
	if !e.st.Offering && e.st.PfToken == "" {
		return
	}
	run("/sbin/pfctl", "-a", exitAnchor, "-F", "all")
	if e.st.PfToken != "" {
		run("/sbin/pfctl", "-X", e.st.PfToken)
	}
	if e.st.FwdWas == "0" || e.st.FwdWas == "" {
		run("/usr/sbin/sysctl", "-w", "net.inet.ip.forwarding=0")
	}
	if e.st.Offering {
		fmt.Println("No longer an exit node")
	}
	e.st.Offering, e.st.NatIface, e.st.PfToken, e.st.FwdWas = false, "", "", ""
	e.save()
}

// pingOK: the exit answers through the tunnel within a second.
func pingOK(ip string) bool {
	return exec.Command("/sbin/ping", "-c", "1", "-t", "1", "-q", ip).Run() == nil
}
