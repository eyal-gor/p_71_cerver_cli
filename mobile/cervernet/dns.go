package cervernet

// A name server inside the phone app, the way Tailscale's MagicDNS works:
// the phone's VPN settings point name lookups at 100.100.100.100, which only
// exists inside this app. Names of your devices (mac-mini, mac-mini.cerver)
// are answered from the list the engine already has; every other name is
// passed on to a public resolver over the phone's normal connection (this
// app is left out of its own VPN, so that lookup doesn't loop back here).

import (
	"encoding/binary"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.zx2c4.com/wireguard/tun"
)

// DNSAddr is the name server's address, on every phone.
var DNSAddr = netip.MustParseAddr("100.100.100.100")

// Safety levels (the same as the Mac's, cmd/addons.go): where lookups for
// everything that isn't one of your devices go.
var safety = map[string][]string{
	"off":     {"1.1.1.1:53", "8.8.8.8:53"},
	"malware": {"1.1.1.2:53", "1.0.0.2:53"},
	"adult":   {"1.1.1.3:53", "1.0.0.3:53"},
	"kids":    {"185.228.168.168:53", "185.228.169.168:53"},
}

var (
	upMu      sync.RWMutex
	level     = "off"
	upstreams = safety["off"]
)

// setLevel picks the safety level chosen in the app (unknown: off).
func setLevel(l string) {
	ups, ok := safety[l]
	if !ok {
		l, ups = "off", safety["off"]
	}
	upMu.Lock()
	level, upstreams = l, ups
	upMu.Unlock()
}

func currentLevel() (string, []string) {
	upMu.RLock()
	defer upMu.RUnlock()
	return level, upstreams
}

type names struct {
	mu sync.RWMutex
	m  map[string]netip.Addr // "mac-mini" → 100.96.0.1 (no ".cerver")
}

func (n *names) set(m map[string]netip.Addr) { n.mu.Lock(); n.m = m; n.mu.Unlock() }

// lookup takes "mac-mini.cerver." or "mac-mini." and says whether it's ours.
func (n *names) lookup(q string) (netip.Addr, bool, bool) {
	q = strings.TrimSuffix(strings.ToLower(q), ".")
	short, isCerver := strings.CutSuffix(q, ".cerver")
	n.mu.RLock()
	defer n.mu.RUnlock()
	a, ok := n.m[short]
	if !isCerver && strings.Contains(q, ".") {
		return netip.Addr{}, false, false // an internet name
	}
	return a, ok, true
}

// dnsTUN answers lookups sent to DNSAddr before they reach WireGuard.
type dnsTUN struct {
	tun.Device
	names *names
	wmu   sync.Mutex
}

func (t *dnsTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for {
		n, err := t.Device.Read(bufs, sizes, offset)
		if n == 0 || err != nil {
			return n, err
		}
		kept := 0
		for i := 0; i < n; i++ {
			p := bufs[i][offset : offset+sizes[i]]
			if q, ok := dnsQuery(p); ok {
				pkt := append([]byte(nil), p...)
				go t.answer(pkt, q)
				continue
			}
			if kept != i {
				bufs[kept], bufs[i] = bufs[i], bufs[kept]
				sizes[kept] = sizes[i]
			}
			kept++
		}
		if kept > 0 {
			return kept, nil
		}
	}
}

// dnsQuery returns the UDP payload of an IPv4 packet to DNSAddr:53.
func dnsQuery(p []byte) ([]byte, bool) {
	if len(p) < 28 || p[0]>>4 != 4 || p[9] != 17 {
		return nil, false
	}
	ihl := int(p[0]&15) * 4
	if len(p) < ihl+8 || netip.AddrFrom4([4]byte(p[16:20])) != DNSAddr || binary.BigEndian.Uint16(p[ihl+2:]) != 53 {
		return nil, false
	}
	return p[ihl+8:], true
}

func (t *dnsTUN) answer(pkt, query []byte) {
	resp := t.resolve(query)
	if resp == nil {
		return
	}
	ihl := int(pkt[0]&15) * 4
	src := [4]byte(pkt[12:16])
	srcPort := binary.BigEndian.Uint16(pkt[ihl:])
	const off = 16
	out := make([]byte, off+20+8+len(resp))
	ip := out[off:]
	ip[0], ip[8], ip[9] = 0x45, 64, 17
	binary.BigEndian.PutUint16(ip[2:], uint16(20+8+len(resp)))
	copy(ip[12:16], DNSAddr.AsSlice())
	copy(ip[16:20], src[:])
	binary.BigEndian.PutUint16(ip[10:], checksum(ip[:20]))
	udp := ip[20:]
	binary.BigEndian.PutUint16(udp[0:], 53)
	binary.BigEndian.PutUint16(udp[2:], srcPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(8+len(resp)))
	copy(udp[8:], resp) // UDP checksum 0: optional over IPv4
	t.wmu.Lock()
	t.Device.Write([][]byte{out}, off)
	t.wmu.Unlock()
}

func checksum(b []byte) uint16 {
	var s uint32
	for i := 0; i+1 < len(b); i += 2 {
		s += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	for s > 0xffff {
		s = s>>16 + s&0xffff
	}
	return ^uint16(s)
}

// resolve answers our names here and forwards the rest.
func (t *dnsTUN) resolve(query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	addr, found, ours := t.names.lookup(q.Name.String())
	if !ours {
		return forward(query)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true})
	if !found {
		b = dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionDesired: h.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeNameError})
	}
	b.EnableCompression()
	b.StartQuestions()
	b.Question(q)
	if found && q.Type == dnsmessage.TypeA {
		b.StartAnswers()
		b.AResource(dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: 60}, dnsmessage.AResource{A: addr.As4()})
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

func forward(query []byte) []byte {
	_, ups := currentLevel()
	for _, up := range ups {
		c, err := net.DialTimeout("udp", up, 3*time.Second)
		if err != nil {
			continue
		}
		c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(query); err == nil {
			buf := make([]byte, 4096)
			if n, err := c.Read(buf); err == nil {
				c.Close()
				return buf[:n]
			}
		}
		c.Close()
	}
	return nil
}
