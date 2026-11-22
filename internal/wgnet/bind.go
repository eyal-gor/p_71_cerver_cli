// Package wgnet is Cerver net's device side: a WireGuard interface whose
// peers (your other devices) and their addresses come from the Cerver
// gateway. See cmd/net.go for the command and the gateway's v2/net/service.ts
// for the coordinator.
package wgnet

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.zx2c4.com/wireguard/conn"
)

// Bind is a WireGuard bind on one UDP socket that also asks STUN servers what
// this socket looks like from outside. Because it is the same socket
// WireGuard sends from, the outside address STUN reports is the one peers can
// actually reach (and the hole our own outgoing packets open in the router).
type Bind struct {
	mu      sync.Mutex
	conn    *net.UDPConn
	waiting map[[12]byte]chan netip.AddrPort
	probes  map[[12]byte]chan struct{}
	relay   RelaySender
	relayIn chan relayPacket
	done    chan struct{}
	// directOff: talk only through the relay. Direct WireGuard packets and
	// probes are ignored, so the other side moves to the relay too.
	directOff atomic.Bool
}

// SetDirectOff turns direct connections off (relay only) or back on.
func (b *Bind) SetDirectOff(off bool) { b.directOff.Store(off) }

func NewBind() *Bind {
	return &Bind{waiting: map[[12]byte]chan netip.AddrPort{}, probes: map[[12]byte]chan struct{}{}, relayIn: make(chan relayPacket, 256)}
}

type endpoint struct{ dst netip.AddrPort }

func (e *endpoint) ClearSrc()           {}
func (e *endpoint) SrcToString() string { return "" }
func (e *endpoint) DstToString() string { return e.dst.String() }
func (e *endpoint) DstToBytes() []byte  { b, _ := e.dst.MarshalBinary(); return b }
func (e *endpoint) DstIP() netip.Addr   { return e.dst.Addr() }
func (e *endpoint) SrcIP() netip.Addr   { return netip.Addr{} }

func (b *Bind) Open(port uint16) ([]conn.ReceiveFunc, uint16, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn != nil {
		return nil, 0, conn.ErrBindAlreadyOpen
	}
	c, err := net.ListenUDP("udp4", &net.UDPAddr{Port: int(port)})
	if err != nil {
		return nil, 0, err
	}
	b.conn = c
	b.done = make(chan struct{})
	actual := uint16(c.LocalAddr().(*net.UDPAddr).Port)
	return []conn.ReceiveFunc{b.receive(c), b.receiveRelay(b.done)}, actual, nil
}

func (b *Bind) receive(c *net.UDPConn) conn.ReceiveFunc {
	return func(packets [][]byte, sizes []int, eps []conn.Endpoint) (int, error) {
		for {
			n, from, err := c.ReadFromUDPAddrPort(packets[0])
			if err != nil {
				return 0, err
			}
			if b.handleSTUN(packets[0][:n]) || b.handleProbe(c, packets[0][:n], from) {
				continue
			}
			if b.directOff.Load() {
				continue // relay only: drop what arrives directly
			}
			sizes[0] = n
			eps[0] = &endpoint{dst: netip.AddrPortFrom(from.Addr().Unmap(), from.Port())}
			return 1, nil
		}
	}
}

func (b *Bind) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return nil
	}
	err := b.conn.Close()
	b.conn = nil
	close(b.done)
	return err
}

func (b *Bind) SetMark(uint32) error { return nil }
func (b *Bind) BatchSize() int       { return 1 }

func (b *Bind) Send(bufs [][]byte, ep conn.Endpoint) error {
	b.mu.Lock()
	c, relay := b.conn, b.relay
	b.mu.Unlock()
	if c == nil {
		return net.ErrClosed
	}
	if re, ok := ep.(*relayEndpoint); ok {
		if relay == nil {
			return errors.New("no relay")
		}
		for _, buf := range bufs {
			if err := relay.SendTo(re.key, buf); err != nil {
				return err
			}
		}
		return nil
	}
	e, ok := ep.(*endpoint)
	if !ok {
		return conn.ErrWrongEndpointType
	}
	for _, buf := range bufs {
		if _, err := c.WriteToUDPAddrPort(buf, e.dst); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bind) ParseEndpoint(s string) (conn.Endpoint, error) {
	if e, ok := parseRelayEndpoint(s); ok {
		return e, nil
	}
	ap, err := netip.ParseAddrPort(s)
	if err != nil {
		return nil, err
	}
	return &endpoint{dst: ap}, nil
}

// Port is the local UDP port, or 0 when closed.
func (b *Bind) Port() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.conn == nil {
		return 0
	}
	return b.conn.LocalAddr().(*net.UDPAddr).Port
}

// ── STUN (RFC 5389): one binding request, read XOR-MAPPED-ADDRESS ──

const stunCookie = 0x2112A442

func isSTUN(p []byte) bool {
	return len(p) >= 20 && p[0]&0xC0 == 0 && binary.BigEndian.Uint32(p[4:8]) == stunCookie
}

func (b *Bind) handleSTUN(p []byte) bool {
	if !isSTUN(p) {
		return false
	}
	var tx [12]byte
	copy(tx[:], p[8:20])
	b.mu.Lock()
	ch := b.waiting[tx]
	b.mu.Unlock()
	if ch != nil {
		if ap, ok := parseMapped(p); ok {
			select {
			case ch <- ap:
			default:
			}
		}
	}
	return true
}

func parseMapped(p []byte) (netip.AddrPort, bool) {
	if binary.BigEndian.Uint16(p[0:2]) != 0x0101 { // binding success response
		return netip.AddrPort{}, false
	}
	attrs := p[20:]
	for len(attrs) >= 4 {
		typ, l := binary.BigEndian.Uint16(attrs[0:2]), int(binary.BigEndian.Uint16(attrs[2:4]))
		if len(attrs) < 4+l {
			break
		}
		v := attrs[4 : 4+l]
		if (typ == 0x0020 || typ == 0x0001) && l >= 8 && v[1] == 0x01 { // (XOR-)MAPPED-ADDRESS, IPv4
			port := binary.BigEndian.Uint16(v[2:4])
			ip := [4]byte{v[4], v[5], v[6], v[7]}
			if typ == 0x0020 {
				port ^= stunCookie >> 16
				var c [4]byte
				binary.BigEndian.PutUint32(c[:], stunCookie)
				for i := range ip {
					ip[i] ^= c[i]
				}
			}
			return netip.AddrPortFrom(netip.AddrFrom4(ip), port), true
		}
		attrs = attrs[4+((l+3)&^3):]
	}
	return netip.AddrPort{}, false
}

// STUN asks one server how this socket looks from outside.
func (b *Bind) STUN(server string, timeout time.Duration) (netip.AddrPort, error) {
	ua, err := net.ResolveUDPAddr("udp4", server)
	if err != nil {
		return netip.AddrPort{}, err
	}
	var tx [12]byte
	rand.Read(tx[:])
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], 0x0001)
	binary.BigEndian.PutUint32(req[4:8], stunCookie)
	copy(req[8:], tx[:])
	ch := make(chan netip.AddrPort, 1)
	b.mu.Lock()
	c := b.conn
	b.waiting[tx] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.waiting, tx); b.mu.Unlock() }()
	if c == nil {
		return netip.AddrPort{}, errors.New("not open")
	}
	deadline := time.After(timeout)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := c.WriteToUDP(req, ua); err != nil {
			return netip.AddrPort{}, err
		}
		select {
		case ap := <-ch:
			return ap, nil
		case <-time.After(timeout / 3):
		case <-deadline:
			return netip.AddrPort{}, fmt.Errorf("no answer from %s", server)
		}
	}
	return netip.AddrPort{}, fmt.Errorf("no answer from %s", server)
}

// ── Direct-path probe ──
//
// While two devices talk through the backup relay, each checks now and then
// whether the direct path works again: a tiny ping on the WireGuard socket to
// the other device's direct address, answered by its Bind. It is not a
// WireGuard packet (those start with a type byte 1-4) and not STUN (whose
// first two bits are 0), so neither is confused by it. WireGuard still
// authenticates everything once traffic moves there.

var probeMagic = [8]byte{'C', 'V', 'R', 'P', 'R', 'O', 'B', 'E'}

const probeLen = 8 + 1 + 12

func (b *Bind) handleProbe(c *net.UDPConn, p []byte, from netip.AddrPort) bool {
	if len(p) != probeLen || [8]byte(p[:8]) != probeMagic {
		return false
	}
	if b.directOff.Load() {
		return true // relay only: don't tell anyone the direct path works
	}
	switch p[8] {
	case 1: // ping: answer from the same socket, so the answer proves the path
		reply := make([]byte, probeLen)
		copy(reply, p)
		reply[8] = 2
		c.WriteToUDPAddrPort(reply, from)
	case 2: // pong
		var tx [12]byte
		copy(tx[:], p[9:])
		b.mu.Lock()
		ch := b.probes[tx]
		b.mu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}
	return true
}

// Probe reports whether the device at addr answers directly, and how fast.
func (b *Bind) Probe(addr netip.AddrPort, timeout time.Duration) (time.Duration, bool) {
	var tx [12]byte
	rand.Read(tx[:])
	ping := make([]byte, probeLen)
	copy(ping, probeMagic[:])
	ping[8] = 1
	copy(ping[9:], tx[:])
	ch := make(chan struct{}, 1)
	b.mu.Lock()
	c := b.conn
	b.probes[tx] = ch
	b.mu.Unlock()
	defer func() { b.mu.Lock(); delete(b.probes, tx); b.mu.Unlock() }()
	if c == nil {
		return 0, false
	}
	start := time.Now()
	deadline := time.After(timeout)
	for attempt := 0; attempt < 3; attempt++ {
		if _, err := c.WriteToUDPAddrPort(ping, addr); err != nil {
			return 0, false
		}
		select {
		case <-ch:
			return time.Since(start), true
		case <-time.After(timeout / 3):
		case <-deadline:
			return 0, false
		}
	}
	return 0, false
}
