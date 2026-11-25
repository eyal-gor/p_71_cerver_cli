package wgnet

import (
	"encoding/binary"
	"net/netip"
	"sync"
	"time"
)

// Filter is this device's access rules: which of your other devices may open
// connections to it, and on which ports. It runs on the device itself, on
// every packet coming out of the tunnel, so a rule holds even if another of
// your devices (or Cerver's server) is compromised: the server can only hand
// out rules, this code decides.
//
// Open (the default, no rules set) lets every device on your network in.
// Restricted lets in:
//   - new connections from a device and port a rule names,
//   - replies to connections this device opened itself,
//   - ICMP errors, and pings from any device that has a rule.
//
// IPv4 only, like the network.
type Filter struct {
	mu    sync.Mutex
	open  bool
	rules map[[4]byte][]PortRange // nil slice = every port
	flows map[flowKey]time.Time
	swept time.Time
}

// PortRange is inclusive; a single port is {p, p}.
type PortRange struct {
	Lo uint16 `json:"lo"`
	Hi uint16 `json:"hi"`
}

// AccessRule: connections from the device at From, to these ports (none = all).
type AccessRule struct {
	From  string      `json:"from_ip"`
	Ports []PortRange `json:"ports"`
}

type flowKey struct {
	proto      byte
	localPort  uint16
	remote     [4]byte
	remotePort uint16
}

const (
	flowIdle     = 30 * time.Minute
	maxFlows     = 1 << 16
	protoICMP    = 1
	protoTCP     = 6
	protoUDP     = 17
	icmpEchoReq  = 8
	icmpEchoRepl = 0
)

func NewFilter() *Filter { return &Filter{open: true, flows: map[flowKey]time.Time{}} }

// Set replaces the rules. open = true lets everyone in (rules ignored).
func (f *Filter) Set(open bool, rules []AccessRule) {
	m := map[[4]byte][]PortRange{}
	for _, r := range rules {
		a, err := netip.ParseAddr(r.From)
		if err != nil || !a.Is4() {
			continue
		}
		k := a.As4()
		if prev, seen := m[k]; seen && prev == nil {
			continue // already every port
		}
		if len(r.Ports) == 0 {
			m[k] = nil
		} else {
			m[k] = append(m[k], r.Ports...)
		}
	}
	f.mu.Lock()
	f.open, f.rules = open, m
	f.mu.Unlock()
}

// Open reports whether everyone on the network may connect (no rules).
func (f *Filter) Open() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

// parse returns protocol, source, destination and (for TCP/UDP) ports, or ok=false.
func parse(p []byte) (proto byte, src, dst [4]byte, sport, dport uint16, icmpType byte, ok bool) {
	if len(p) < 20 || p[0]>>4 != 4 {
		return
	}
	ihl := int(p[0]&15) * 4
	if ihl < 20 || len(p) < ihl {
		return
	}
	proto = p[9]
	copy(src[:], p[12:16])
	copy(dst[:], p[16:20])
	// Only the first fragment carries ports; later fragments follow it.
	if binary.BigEndian.Uint16(p[6:8])&0x1fff != 0 {
		return proto, src, dst, 0, 0, 0, true
	}
	l4 := p[ihl:]
	switch proto {
	case protoTCP, protoUDP:
		if len(l4) < 4 {
			return
		}
		sport, dport = binary.BigEndian.Uint16(l4[0:2]), binary.BigEndian.Uint16(l4[2:4])
	case protoICMP:
		if len(l4) < 1 {
			return
		}
		icmpType = l4[0]
	}
	return proto, src, dst, sport, dport, icmpType, true
}

// Outbound records a packet this device sends, so its replies get back in.
func (f *Filter) Outbound(p []byte) {
	proto, _, dst, sport, dport, icmpType, ok := parse(p)
	if !ok {
		return
	}
	var k flowKey
	switch {
	case proto == protoTCP || proto == protoUDP:
		k = flowKey{proto, sport, dst, dport}
	case proto == protoICMP && icmpType == icmpEchoReq:
		k = flowKey{protoICMP, 0, dst, 0}
	default:
		return
	}
	now := time.Now()
	f.mu.Lock()
	if f.open {
		f.mu.Unlock()
		return // nothing to check replies against
	}
	if len(f.flows) >= maxFlows || now.Sub(f.swept) > time.Minute {
		for fk, t := range f.flows {
			if now.Sub(t) > flowIdle || len(f.flows) >= maxFlows {
				delete(f.flows, fk)
			}
		}
		f.swept = now
	}
	f.flows[k] = now
	f.mu.Unlock()
}

// Inbound decides whether a packet from the tunnel may reach this device.
func (f *Filter) Inbound(p []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.open {
		return true
	}
	proto, src, _, sport, dport, icmpType, ok := parse(p)
	if !ok {
		return false
	}
	ports, hasRule := f.rules[src]
	switch proto {
	case protoTCP, protoUDP:
		if sport == 0 && dport == 0 {
			return true // a later fragment; its first fragment was checked
		}
		if t, ok := f.flows[flowKey{proto, dport, src, sport}]; ok && time.Since(t) < flowIdle {
			return true // a reply to something we opened
		}
		if !hasRule {
			return false
		}
		if ports == nil {
			return true
		}
		for _, r := range ports {
			if dport >= r.Lo && dport <= r.Hi {
				return true
			}
		}
		return false
	case protoICMP:
		switch icmpType {
		case icmpEchoRepl:
			_, sent := f.flows[flowKey{protoICMP, 0, src, 0}]
			return sent
		case icmpEchoReq:
			return hasRule
		default:
			return true // unreachable, time exceeded: errors about our own traffic
		}
	}
	return hasRule && ports == nil
}
