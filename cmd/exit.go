package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// Exit nodes from the terminal (and for agents):
//
//	cerver exit                          who offers, who uses which
//	cerver exit offer <computer>         let your devices send their internet through it
//	cerver exit stop <computer>          stop offering it (devices using it go direct)
//	cerver exit use <computer> <exit>    send <computer>'s internet through <exit>
//	cerver exit off <computer>           <computer> uses its own connection again
func Exit(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, gw, err := remoteNodes(ctx)
	if err != nil {
		return err
	}
	find := func(q string) (*remoteNode, error) {
		var hits []*remoteNode
		for i := range nodes {
			n := &nodes[i]
			if strings.EqualFold(n.Name, q) || n.IP == q {
				return n, nil
			}
			if strings.Contains(wgHost(n.Name), wgHost(q)) {
				hits = append(hits, n)
			}
		}
		if len(hits) == 1 {
			return hits[0], nil
		}
		return nil, fmt.Errorf("no single computer %q on your network", q)
	}
	put := func(n *remoteNode, body map[string]any) error {
		var out struct {
			Say string `json:"say"`
		}
		if err := gw.Do(ctx, "PUT", "/v2/net/nodes/"+n.IP+"/exit", body, &out); err != nil {
			return err
		}
		fmt.Println(out.Say)
		return nil
	}
	if len(args) == 0 || args[0] == "--json" {
		if len(args) > 0 {
			return json.NewEncoder(os.Stdout).Encode(nodes)
		}
		names := map[string]string{}
		for _, n := range nodes {
			names[n.IP] = n.Name
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "COMPUTER\tEXIT NODE\tINTERNET GOES\tNOW")
		for _, n := range nodes {
			offer, via, now := "-", "directly", ""
			if n.ExitOffer {
				offer = "offered"
			}
			if n.ExitVia != "" {
				via = "through " + names[n.ExitVia]
			}
			var a struct {
				Exit struct {
					Offering bool   `json:"offering"`
					Active   bool   `json:"active"`
					Why      string `json:"why"`
				} `json:"exit"`
			}
			_ = json.Unmarshal(n.Applied, &a)
			switch {
			case a.Exit.Why != "":
				now = a.Exit.Why
			case n.ExitVia != "" && a.Exit.Active:
				now = "in effect"
			case n.ExitOffer && a.Exit.Offering:
				now = "ready"
			case n.ExitOffer || n.ExitVia != "":
				now = "applying (or the computer needs a Cerver update)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n.Name, offer, via, now)
		}
		return w.Flush()
	}
	switch args[0] {
	case "offer", "stop", "off":
		if len(args) != 2 {
			return errors.New("usage: cerver exit " + args[0] + " <computer>")
		}
		n, err := find(args[1])
		if err != nil {
			return err
		}
		switch args[0] {
		case "offer":
			return put(n, map[string]any{"offer": true})
		case "stop":
			return put(n, map[string]any{"offer": false})
		default:
			return put(n, map[string]any{"via": nil})
		}
	case "use":
		if len(args) != 3 {
			return errors.New("usage: cerver exit use <computer> <exit computer>")
		}
		n, err := find(args[1])
		if err != nil {
			return err
		}
		ex, err := find(args[2])
		if err != nil {
			return err
		}
		return put(n, map[string]any{"via": ex.IP})
	}
	return errors.New("usage: cerver exit [offer|stop|use|off] …")
}
