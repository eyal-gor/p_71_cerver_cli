package wgnet

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/device"
)

// STUNServers answer "what is my outside address?". Two, so one being down
// doesn't matter, and so differing answers reveal a router that changes the
// port per destination (then direct connections usually fail).
var STUNServers = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}

// Key is a WireGuard key pair.
type Key struct{ Private, Public [32]byte }

func NewKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k.Private[:]); err != nil {
		return k, err
	}
	k.Private[0] &= 248
	k.Private[31] = (k.Private[31] & 127) | 64
	pub, err := curve25519.X25519(k.Private[:], curve25519.Basepoint)
	if err != nil {
		return k, err
	}
	copy(k.Public[:], pub)
	return k, nil
}

// LoadOrCreateKey keeps the device's key in a file, so its address stays the same.
func LoadOrCreateKey(path string) (Key, error) {
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err == nil && len(raw) == 32 {
			var k Key
			copy(k.Private[:], raw)
			pub, err := curve25519.X25519(k.Private[:], curve25519.Basepoint)
			if err == nil {
				copy(k.Public[:], pub)
				return k, nil
			}
		}
	}
	k, err := NewKey()
	if err != nil {
		return k, err
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	return k, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(k.Private[:])+"\n"), 0o600)
}

func (k Key) PublicB64() string { return base64.StdEncoding.EncodeToString(k.Public[:]) }

// Peer is another device, as the gateway describes it.
type Peer struct {
	Name      string   `json:"name"`
	PublicKey string   `json:"public_key"`
	IP        string   `json:"ip"`
	Endpoints []string `json:"endpoints"`
	Online    bool     `json:"online"`
}

// CheckInRequest and CheckInAnswer are POST /v2/net/nodes.
type CheckInRequest struct {
	PublicKey string   `json:"public_key"`
	Name      string   `json:"name"`
	Endpoints []string `json:"endpoints"`
	OS        string   `json:"os"`
}
type CheckInAnswer struct {
	IP          string `json:"ip"`
	Prefix      string `json:"prefix"`
	PollSeconds int    `json:"poll_seconds"`
	Peers       []Peer `json:"peers"`
}

type peerState struct {
	peer       Peer
	candidates []string
	idx        int
	setAt      time.Time
	current    string
}

// Node runs the WireGuard device's peer list from the gateway's answers.
type Node struct {
	Dev  *device.Device
	Bind *Bind
	Key  Key
	Name string
	OS   string
	// CheckIn calls the gateway (injectable for tests).
	CheckIn func(context.Context, CheckInRequest) (*CheckInAnswer, error)
	// Log prints a line for the person running it.
	Log func(format string, a ...any)
	// LocalOnly skips STUN (tests on one machine).
	LocalOnly bool
	// Relay, when set, is every peer's last address to try.
	Relay bool
	// TryFor is how long one address gets before trying the next (default
	// 12 s). WireGuard retries a handshake every 5 s, so under ~6 s an address
	// isn't really tried.
	TryFor time.Duration

	mu      sync.Mutex
	peers   map[string]*peerState
	public  netip.Addr
	Answer  *CheckInAnswer
	natWarn bool
}

// Start sets the key and opens the socket.
func (n *Node) Start() error {
	if err := n.Dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=0\n", hex.EncodeToString(n.Key.Private[:]))); err != nil {
		return err
	}
	return n.Dev.Up()
}

// Endpoints are the addresses others might reach this device at: outside
// (from STUN) first, then the local network.
func (n *Node) Endpoints() []string {
	port := n.Bind.Port()
	var out []string
	add := func(s string) {
		for _, e := range out {
			if e == s {
				return
			}
		}
		out = append(out, s)
	}
	if !n.LocalOnly {
		var seen []netip.AddrPort
		for _, s := range STUNServers {
			if ap, err := n.Bind.STUN(s, 3*time.Second); err == nil {
				seen = append(seen, ap)
				add(ap.String())
			}
		}
		n.mu.Lock()
		if len(seen) > 0 {
			n.public = seen[0].Addr()
		}
		if len(seen) == 2 && seen[0].Port() != seen[1].Port() && !n.natWarn {
			n.natWarn = true
			n.Log("Note: this network's router changes the outside port per destination; direct connections to devices on other networks may fail.")
		}
		n.mu.Unlock()
	}
	for _, ip := range localIPv4s() {
		add(netip.AddrPortFrom(ip, uint16(port)).String())
	}
	return out
}

func localIPv4s() []netip.Addr {
	var out []netip.Addr
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		ip := pfx.Addr()
		if !ip.Is4() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if netip.MustParsePrefix("100.96.0.0/16").Contains(ip) {
			continue // our own network
		}
		out = append(out, ip)
	}
	return out
}

// Order tries the likeliest address first: a peer behind the same router
// (same outside IP) is reached on its local address; anyone else on its
// outside address. Pure, for tests.
func Order(endpoints []string, myPublic netip.Addr) []string {
	type c struct {
		s    string
		rank int
	}
	var cs []c
	samePlace := false
	for _, e := range endpoints {
		if ap, err := netip.ParseAddrPort(e); err == nil && myPublic.IsValid() && ap.Addr() == myPublic {
			samePlace = true
		}
	}
	for i, e := range endpoints {
		ap, err := netip.ParseAddrPort(e)
		if err != nil {
			continue
		}
		private := ap.Addr().IsPrivate()
		rank := i
		if private != samePlace {
			rank += 100
		}
		cs = append(cs, c{e, rank})
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].rank < cs[j].rank })
	out := make([]string, len(cs))
	for i, x := range cs {
		out[i] = x.s
	}
	return out
}

// Tick checks in once and applies the answer. Call it every PollSeconds.
func (n *Node) Tick(ctx context.Context) error {
	ans, err := n.CheckIn(ctx, CheckInRequest{PublicKey: n.Key.PublicB64(), Name: n.Name, Endpoints: n.Endpoints(), OS: n.OS})
	if err != nil {
		return err
	}
	n.mu.Lock()
	n.Answer = ans
	n.mu.Unlock()
	return n.apply(ans.Peers)
}

func (n *Node) apply(peers []Peer) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.peers == nil {
		n.peers = map[string]*peerState{}
	}
	handshakes := n.handshakes()
	var cfg strings.Builder
	keep := map[string]bool{}
	for _, p := range peers {
		raw, err := base64.StdEncoding.DecodeString(p.PublicKey)
		if err != nil || len(raw) != 32 || p.PublicKey == n.Key.PublicB64() {
			continue
		}
		hexKey := hex.EncodeToString(raw)
		keep[hexKey] = true
		st := n.peers[hexKey]
		if st == nil {
			st = &peerState{}
			n.peers[hexKey] = st
			n.Log("Peer %s at %s", p.Name, p.IP)
		}
		st.peer = p
		st.candidates = Order(p.Endpoints, n.public)
		if n.Relay {
			var k [32]byte
			copy(k[:], raw)
			st.candidates = append(st.candidates, RelayAddr(k))
		}
		fmt.Fprintf(&cfg, "public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/32\npersistent_keepalive_interval=25\n", hexKey, p.IP)
		// Keep the address that works (WireGuard follows a peer that moves).
		// Only pick another when nothing has answered for a while.
		connected := time.Since(handshakes[hexKey]) < 3*time.Minute
		if len(st.candidates) > 0 && !connected && (st.current == "" || time.Since(st.setAt) > n.tryFor()) {
			if st.current != "" {
				st.idx = (st.idx + 1) % len(st.candidates)
			}
			next := st.candidates[st.idx%len(st.candidates)]
			if next != st.current {
				st.current, st.setAt = next, time.Now()
				fmt.Fprintf(&cfg, "endpoint=%s\n", next)
			} else {
				st.setAt = time.Now()
			}
		}
	}
	for hexKey, st := range n.peers {
		if !keep[hexKey] {
			fmt.Fprintf(&cfg, "public_key=%s\nremove=true\n", hexKey)
			delete(n.peers, hexKey)
			n.Log("Peer %s left", st.peer.Name)
		}
	}
	if cfg.Len() == 0 {
		return nil
	}
	return n.Dev.IpcSet(cfg.String())
}

func (n *Node) tryFor() time.Duration {
	if n.TryFor > 0 {
		return n.TryFor
	}
	return 12 * time.Second
}

// Retry moves peers that aren't answering on to their next address, without
// checking in again. Call it every few seconds so a blocked direct path
// gives way to the relay quickly.
func (n *Node) Retry() error {
	n.mu.Lock()
	ans := n.Answer
	n.mu.Unlock()
	if ans == nil {
		return nil
	}
	return n.apply(ans.Peers)
}

// handshakes reads each peer's last handshake time from the device.
func (n *Node) handshakes() map[string]time.Time {
	t, _ := n.deviceState()
	return t
}

// deviceState reads each peer's last handshake and current address from WireGuard.
func (n *Node) deviceState() (map[string]time.Time, map[string]string) {
	out, eps := map[string]time.Time{}, map[string]string{}
	s, err := n.Dev.IpcGet()
	if err != nil {
		return out, eps
	}
	var cur string
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "public_key":
			cur = v
		case "last_handshake_time_sec":
			if sec, err := strconv.ParseInt(v, 10, 64); err == nil && sec > 0 {
				out[cur] = time.Unix(sec, 0)
			}
		case "endpoint":
			eps[cur] = v
		}
	}
	return out, eps
}

// Status is one line per peer for the person watching.
type PeerStatus struct {
	Name, IP, Endpoint string
	Connected          bool
	LastHandshake      time.Time
}

func (n *Node) Status() []PeerStatus {
	n.mu.Lock()
	defer n.mu.Unlock()
	hs, eps := n.deviceState()
	var out []PeerStatus
	for hexKey, st := range n.peers {
		t := hs[hexKey]
		ep := eps[hexKey] // where WireGuard is actually sending (it follows a peer that moves)
		if ep == "" {
			ep = st.current
		}
		if strings.HasPrefix(ep, relayPrefix) {
			ep = "Cerver relay"
		}
		out = append(out, PeerStatus{Name: st.peer.Name, IP: st.peer.IP, Endpoint: ep, Connected: !t.IsZero() && time.Since(t) < 3*time.Minute, LastHandshake: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].IP < out[j].IP })
	return out
}
