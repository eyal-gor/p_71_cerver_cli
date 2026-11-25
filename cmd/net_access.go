package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/eyal-gor/p_71_cerver_cli/internal/wgnet"
)

// `cerver net access` sets which of your devices may connect to one of them.
//
//	cerver net access mac-mini                      # show
//	cerver net access mac-mini all                  # every device (the default)
//	cerver net access mac-mini only phone:screen macbook
//
// A device with "only" accepts new connections from the listed devices, on
// the listed ports (none = every port); replies to its own connections always
// get back. The device enforces this itself.
func netAccess(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cerver net access <device> [all | only <device>[:port,...] ...]\n  ports: a number, or screen (5900), ssh (22), files (445), web (80,443)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	gw, err := netGateway(ctx)
	if err != nil {
		return err
	}
	target := args[0]
	if len(args) == 1 {
		return showAccess(ctx, gw.Do, target)
	}
	var body map[string]any
	switch args[1] {
	case "all":
		body = map[string]any{"mode": "all"}
	case "only":
		from := []map[string]any{}
		for _, a := range args[2:] {
			dev, ports, err := parseAccessArg(a)
			if err != nil {
				return err
			}
			e := map[string]any{"device": dev}
			if len(ports) > 0 {
				e["ports"] = ports
			}
			from = append(from, e)
		}
		body = map[string]any{"mode": "only", "from": from}
	default:
		return fmt.Errorf("say all or only, e.g. cerver net access %s only phone:screen", target)
	}
	var out struct {
		Name   string     `json:"name"`
		Access accessDesc `json:"access"`
		Say    string     `json:"say"`
	}
	if err := gw.Do(ctx, "PUT", "/v2/net/nodes/"+target+"/access", body, &out); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n%s\n", out.Name, out.Access.String(), out.Say)
	return nil
}

type accessDesc struct {
	Mode string `json:"mode"`
	From []struct {
		Name  string `json:"name"`
		Ports []int  `json:"ports"`
	} `json:"from"`
}

func (a accessDesc) String() string {
	if a.Mode != "only" {
		return "every device on your network can connect"
	}
	if len(a.From) == 0 {
		return "no device can connect (it can still reach the others)"
	}
	parts := []string{}
	for _, f := range a.From {
		if len(f.Ports) == 0 {
			parts = append(parts, f.Name+" (everything)")
			continue
		}
		ps := []string{}
		for _, p := range f.Ports {
			ps = append(ps, portName(p))
		}
		parts = append(parts, f.Name+" ("+strings.Join(ps, ", ")+")")
	}
	return "only " + strings.Join(parts, "; ")
}

var serviceNames = map[string][]int{"screen": {5900}, "vnc": {5900}, "ssh": {22}, "files": {445}, "smb": {445}, "web": {80, 443}}

func portName(p int) string {
	switch p {
	case 5900:
		return "screen sharing"
	case 22:
		return "ssh"
	case 445:
		return "files"
	}
	return strconv.Itoa(p)
}

// parseAccessArg reads "phone", "phone:screen" or "phone:22,8080".
func parseAccessArg(a string) (string, []int, error) {
	dev, list, _ := strings.Cut(a, ":")
	if dev == "" {
		return "", nil, fmt.Errorf("missing device name in %q", a)
	}
	var ports []int
	for _, p := range strings.Split(list, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if s, ok := serviceNames[p]; ok {
			ports = append(ports, s...)
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", nil, fmt.Errorf("%q isn't a port or one of screen, ssh, files, web", p)
		}
		ports = append(ports, n)
	}
	return dev, ports, nil
}

func showAccess(ctx context.Context, do func(context.Context, string, string, any, any) error, target string) error {
	var st struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
			Name   string `json:"name"`
			Label  string `json:"label"`
			IP     string `json:"ip"`
			Access *struct {
				From []struct {
					NodeID string `json:"node_id"`
					Ports  []int  `json:"ports"`
				} `json:"from"`
			} `json:"access"`
		} `json:"nodes"`
	}
	if err := do(ctx, "GET", "/v2/net", nil, &st); err != nil {
		return err
	}
	names := map[string]string{}
	for _, n := range st.Nodes {
		names[n.NodeID] = firstNonEmpty(n.Label, n.Name)
	}
	for _, n := range st.Nodes {
		if n.Name != target && n.IP != target && n.Label != target && n.NodeID != target {
			continue
		}
		d := accessDesc{Mode: "all"}
		if n.Access != nil {
			d.Mode = "only"
			for _, f := range n.Access.From {
				d.From = append(d.From, struct {
					Name  string `json:"name"`
					Ports []int  `json:"ports"`
				}{names[f.NodeID], f.Ports})
			}
		}
		fmt.Printf("%s: %s\n", firstNonEmpty(n.Label, n.Name), d.String())
		return nil
	}
	return fmt.Errorf("no device %q in your network", target)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// applyInbound gives the device's filter the rules from its latest check-in.
// An answer without rules (an older gateway) leaves the device open.
func applyInbound(f *wgnet.Filter, ans *wgnet.CheckInAnswer) {
	if ans == nil || ans.Inbound == nil {
		f.Set(true, nil)
		return
	}
	f.Set(ans.Inbound.Open, ans.Inbound.Rules)
}
