package cmd

import "testing"

func TestExitHelpers(t *testing.T) {
	ns := `Routing tables

Internet:
Destination        Gateway            Flags               Netif Expire
0/1                utun4              USc                 utun4
default            192.168.1.1        UGScg                 en0
default            link#22            UCSIg               utun4
100.96/16          utun4              USc                 utun4
`
	if gw, ifc := parseDefault(ns); gw != "192.168.1.1" || ifc != "en0" {
		t.Errorf("parseDefault = %q %q", gw, ifc)
	}
	for ip, want := range map[string]bool{"104.16.1.1": true, "192.168.1.5": false, "10.0.0.1": false, "100.96.0.2": false, "100.70.1.1": false, "127.0.0.1": false, "x": false} {
		if publicIP(ip) != want {
			t.Errorf("publicIP(%s) != %v", ip, want)
		}
	}
	if natRule("en0") != "nat on en0 inet from 100.96.0.0/16 to any -> (en0)\n" {
		t.Error(natRule("en0"))
	}
}
