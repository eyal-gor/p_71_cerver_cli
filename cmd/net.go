package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"

	"github.com/eyal-gor/p_71_cerver_cli/internal/gateway"
	"github.com/eyal-gor/p_71_cerver_cli/internal/infisical"
	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
)

// Net is Cerver net: a private WireGuard network between your own devices.
//
//	sudo cerver net up [--name laptop]   # join: this device gets 100.96.x.y
//	cerver net                           # your devices and their addresses
//	cerver net rm <name|address>         # take a device out
//
// Devices talk to each other directly, encrypted. Cerver's gateway only swaps
// their public keys and current addresses.
func Net(args []string) error {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "up":
		return netUp(args)
	case "", "status", "ls":
		return netStatus()
	case "install":
		return netInstall(args)
	case "uninstall":
		return netUninstall()
	case "rm", "remove":
		if len(args) != 1 {
			return fmt.Errorf("usage: cerver net rm <name|address>")
		}
		return netRemove(args[0])
	default:
		return fmt.Errorf("unknown: cerver net %s (try: up, install, uninstall, status, rm)", sub)
	}
}

func netGateway(ctx context.Context) (*gateway.Client, error) {
	tok, err := infisical.LoadCerverToken(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, fmt.Errorf("no cerver credentials found — run cerver.ai/install.sh first")
	}
	return gateway.New(tok), nil
}

func netStatus() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	var out struct {
		Nodes []struct {
			Name     string   `json:"name"`
			IP       string   `json:"ip"`
			OS       string   `json:"os"`
			Online   bool     `json:"online"`
			LastSeen string   `json:"last_seen"`
			Ends     []string `json:"endpoints"`
		} `json:"nodes"`
	}
	if err := gw.Do(ctx, "GET", "/v2/net", nil, &out); err != nil {
		return err
	}
	if len(out.Nodes) == 0 {
		fmt.Println("No devices in your network yet. On each one: sudo cerver net up")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "DEVICE\tADDRESS\tSTATE\tOS")
	for _, n := range out.Nodes {
		state := "offline"
		if n.Online {
			state = "online"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Name, n.IP, state, n.OS)
	}
	return w.Flush()
}

func netRemove(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	var out struct {
		Removed []string `json:"removed"`
	}
	if err := gw.Do(ctx, "DELETE", "/v2/net/nodes/"+id, nil, &out); err != nil {
		return err
	}
	fmt.Printf("Removed %s\n", strings.Join(out.Removed, ", "))
	return nil
}

func netUp(args []string) error {
	fs := flag.NewFlagSet("net up", flag.ContinueOnError)
	host, _ := os.Hostname()
	name := fs.String("name", strings.TrimSuffix(host, ".local"), "This device's name in your network")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("cerver net runs on macOS and Linux for now")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("making a network interface needs admin rights: sudo cerver net up")
	}
	// Under sudo, keep using the person's own ~/.cerver (key and credentials).
	if su := os.Getenv("SUDO_USER"); su != "" {
		if u, err := user.Lookup(su); err == nil {
			os.Setenv("HOME", u.HomeDir)
		}
	}
	home, _ := os.UserHomeDir()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	key, err := wgnet.LoadOrCreateKey(filepath.Join(home, ".cerver", "net.key"))
	if err != nil {
		return fmt.Errorf("device key: %w", err)
	}

	tunName := "cerver0"
	if runtime.GOOS == "darwin" {
		tunName = "utun"
	}
	tdev, err := tun.CreateTUN(tunName, 1280)
	if err != nil {
		return fmt.Errorf("can't create the network interface: %w", err)
	}
	ifname, _ := tdev.Name()
	self := wgnet.NewSelfTUN(tdev)
	bind := wgnet.NewBind()
	dev := device.NewDevice(self, bind, device.NewLogger(device.LogLevelError, "wireguard: "))
	defer dev.Close()

	relay := &wgnet.RelayClient{URL: gw.BaseURL, Token: gw.Token, PublicB64: key.PublicB64(), Bind: bind,
		Log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) }}
	bind.SetRelay(relay)
	node := &wgnet.Node{
		Dev: dev, Bind: bind, Key: key, Name: *name, OS: runtime.GOOS, Relay: true,
		Log: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
		CheckIn: func(ctx context.Context, r wgnet.CheckInRequest) (*wgnet.CheckInAnswer, error) {
			var ans wgnet.CheckInAnswer
			err := gw.Do(ctx, "POST", "/v2/net/nodes", r, &ans)
			return &ans, err
		},
	}
	if err := node.Start(); err != nil {
		return err
	}
	if err := node.Tick(ctx); err != nil {
		return fmt.Errorf("couldn't join your network: %w", err)
	}
	ip := node.Answer.IP
	self.SetSelf(ip)
	go relay.Run(ctx) // after the first check-in: the relay only admits registered devices
	if err := configureInterface(ifname, ip, node.Answer.Prefix); err != nil {
		return err
	}
	updateNames(*name, node)
	defer wgnet.WriteHosts(hostsFile, "")
	fmt.Printf("%s is on your Cerver network at %s (interface %s). Ctrl-C to leave.\n", *name, ip, ifname)
	if len(node.Answer.Peers) == 0 {
		fmt.Println("No other devices yet. Run `sudo cerver net up` on another one.")
	}

	poll := time.Duration(max(5, node.Answer.PollSeconds)) * time.Second
	tick := time.NewTicker(poll)
	defer tick.Stop()
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	retry := time.NewTicker(3 * time.Second) // move a silent peer on to its next address
	defer retry.Stop()
	shown := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			fmt.Println("\nLeft the network.")
			return nil
		case <-tick.C:
			if err := node.Tick(ctx); err != nil && ctx.Err() == nil {
				fmt.Printf("Couldn't reach Cerver (%v); keeping current peers.\n", err)
			} else {
				updateNames(*name, node)
			}
			for _, s := range node.Status() {
				if s.Connected && !shown[s.IP] {
					shown[s.IP] = true
					fmt.Printf("Connected to %s (%s) via %s\n", s.Name, s.IP, s.Endpoint)
				} else if !s.Connected && shown[s.IP] {
					shown[s.IP] = false
					fmt.Printf("Lost %s (%s); trying again\n", s.Name, s.IP)
				}
			}
		case <-retry.C:
			node.Retry()
		case <-report.C:
			for _, s := range node.Status() {
				if !s.Connected {
					fmt.Printf("Still can't reach %s (%s); last tried %s\n", s.Name, s.IP, s.Endpoint)
				}
			}
		}
	}
}

const hostsFile = "/etc/hosts"

// updateNames makes every device reachable by name (ssh macbook).
func updateNames(self string, node *wgnet.Node) {
	if node.Answer == nil {
		return
	}
	devices := map[string]string{node.Answer.IP: self}
	for _, p := range node.Answer.Peers {
		devices[p.IP] = p.Name
	}
	before, _ := os.ReadFile(hostsFile)
	if err := wgnet.WriteHosts(hostsFile, wgnet.HostsBlock(devices)); err != nil {
		fmt.Printf("Couldn't update device names: %v\n", err)
		return
	}
	if after, _ := os.ReadFile(hostsFile); runtime.GOOS == "darwin" && string(after) != string(before) {
		exec.Command("dscacheutil", "-flushcache").Run()
		exec.Command("killall", "-HUP", "mDNSResponder").Run()
	}
}

// configureInterface gives the interface its address and routes the network to it.
func configureInterface(ifname, ip, prefix string) error {
	var cmds [][]string
	switch runtime.GOOS {
	case "darwin":
		cmds = [][]string{
			{"ifconfig", ifname, "inet", ip, ip, "netmask", "255.255.255.255", "up"},
			{"route", "-q", "-n", "add", "-net", prefix, "-interface", ifname},
		}
	case "linux":
		cmds = [][]string{
			{"ip", "addr", "add", ip + "/16", "dev", ifname},
			{"ip", "link", "set", ifname, "up", "mtu", "1280"},
		}
	}
	for _, c := range cmds {
		if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %v: %s", strings.Join(c, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
