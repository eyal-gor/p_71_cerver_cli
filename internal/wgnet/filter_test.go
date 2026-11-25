package wgnet

import (
	"encoding/binary"
	"testing"
)

// pkt builds a minimal IPv4 packet: proto, src, dst, and ports (TCP/UDP) or ICMP type.
func fpkt(proto byte, src, dst [4]byte, sport, dport uint16, icmpType byte) []byte {
	p := make([]byte, 28)
	p[0] = 0x45
	p[9] = proto
	copy(p[12:16], src[:])
	copy(p[16:20], dst[:])
	if proto == protoICMP {
		p[20] = icmpType
	} else {
		binary.BigEndian.PutUint16(p[20:22], sport)
		binary.BigEndian.PutUint16(p[22:24], dport)
	}
	return p
}

var (
	me    = [4]byte{100, 96, 0, 1}
	phone = [4]byte{100, 96, 0, 3}
	air   = [4]byte{100, 96, 0, 2}
)

func TestFilterOpenByDefault(t *testing.T) {
	f := NewFilter()
	if !f.Inbound(fpkt(protoTCP, air, me, 50000, 22, 0)) {
		t.Fatal("no rules should let everyone in")
	}
}

func TestFilterOnlyNamedDeviceAndPort(t *testing.T) {
	f := NewFilter()
	f.Set(false, []AccessRule{{From: "100.96.0.3", Ports: []PortRange{{5900, 5900}}}})
	if !f.Inbound(fpkt(protoTCP, phone, me, 50000, 5900, 0)) {
		t.Fatal("phone → screen sharing should be allowed")
	}
	if f.Inbound(fpkt(protoTCP, phone, me, 50000, 22, 0)) {
		t.Fatal("phone → ssh should be refused")
	}
	if f.Inbound(fpkt(protoTCP, air, me, 50000, 5900, 0)) {
		t.Fatal("a device with no rule should be refused")
	}
	if !f.Inbound(fpkt(protoICMP, phone, me, 0, 0, icmpEchoReq)) || f.Inbound(fpkt(protoICMP, air, me, 0, 0, icmpEchoReq)) {
		t.Fatal("pings: only from devices with a rule")
	}
}

func TestFilterRepliesToOwnConnections(t *testing.T) {
	f := NewFilter()
	f.Set(false, nil)
	if f.Inbound(fpkt(protoTCP, air, me, 22, 51000, 0)) {
		t.Fatal("unsolicited packet should be refused")
	}
	f.Outbound(fpkt(protoTCP, me, air, 51000, 22, 0)) // we ssh to the air
	if !f.Inbound(fpkt(protoTCP, air, me, 22, 51000, 0)) {
		t.Fatal("reply to our own connection should get in")
	}
	if f.Inbound(fpkt(protoTCP, air, me, 22, 51001, 0)) {
		t.Fatal("a different port is not that connection")
	}
	f.Outbound(fpkt(protoICMP, me, air, 0, 0, icmpEchoReq))
	if !f.Inbound(fpkt(protoICMP, air, me, 0, 0, icmpEchoRepl)) {
		t.Fatal("ping reply should get in")
	}
}

func TestFilterAllPortsAndBackToOpen(t *testing.T) {
	f := NewFilter()
	f.Set(false, []AccessRule{{From: "100.96.0.2"}})
	if !f.Inbound(fpkt(protoUDP, air, me, 1, 60000, 0)) {
		t.Fatal("a rule without ports allows every port")
	}
	f.Set(true, nil)
	if !f.Inbound(fpkt(protoTCP, phone, me, 1, 22, 0)) {
		t.Fatal("open again lets everyone in")
	}
}
