package wgnet

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

func TestOrder(t *testing.T) {
	pub := netip.MustParseAddr("84.229.10.20")
	away := []string{"31.0.0.9:4000", "192.168.1.7:4000"}
	if got := Order(away, pub); got[0] != "31.0.0.9:4000" {
		t.Fatalf("other network: outside address first, got %v", got)
	}
	home := []string{"84.229.10.20:5000", "192.168.1.8:5000"}
	if got := Order(home, pub); got[0] != "192.168.1.8:5000" {
		t.Fatalf("same router: local address first, got %v", got)
	}
}

func TestParseMapped(t *testing.T) {
	// XOR-MAPPED-ADDRESS for 84.229.10.20:41641
	p := make([]byte, 32)
	p[0], p[1] = 0x01, 0x01
	p[3] = 12
	p[4], p[5], p[6], p[7] = 0x21, 0x12, 0xA4, 0x42
	attr := p[20:]
	attr[0], attr[1], attr[3] = 0x00, 0x20, 8
	attr[5] = 0x01
	port := uint16(41641) ^ 0x2112
	attr[6], attr[7] = byte(port>>8), byte(port)
	ip := [4]byte{84, 229, 10, 20}
	c := [4]byte{0x21, 0x12, 0xA4, 0x42}
	for i := range ip {
		attr[8+i] = ip[i] ^ c[i]
	}
	ap, ok := parseMapped(p)
	if !ok || ap.String() != "84.229.10.20:41641" {
		t.Fatalf("got %v %v", ap, ok)
	}
}

// fakeGateway is the coordinator in memory: same rules as v2/net/service.ts.
type fakeGateway struct {
	mu    sync.Mutex
	nodes map[string]*Peer
	n     int
}

func (g *fakeGateway) checkIn(_ context.Context, r CheckInRequest) (*CheckInAnswer, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	me := g.nodes[r.PublicKey]
	if me == nil {
		g.n++
		me = &Peer{PublicKey: r.PublicKey, IP: fmt.Sprintf("100.96.0.%d", g.n)}
		g.nodes[r.PublicKey] = me
	}
	me.Name, me.Endpoints, me.Online = r.Name, r.Endpoints, true
	ans := &CheckInAnswer{IP: me.IP, Prefix: "100.96.0.0/16", PollSeconds: 1}
	for k, p := range g.nodes {
		if k != r.PublicKey {
			ans.Peers = append(ans.Peers, *p)
		}
	}
	return ans, nil
}

type testNode struct {
	*Node
	net *netstack.Net
}

func newTestNode(t *testing.T, g *fakeGateway, name string) *testNode {
	key, _ := NewKey()
	// The address is known only after the first check-in; netstack needs it
	// up front, so ask the fake gateway first (the real command does the same).
	ans, _ := g.checkIn(context.Background(), CheckInRequest{PublicKey: key.PublicB64(), Name: name})
	tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(ans.IP)}, nil, 1280)
	if err != nil {
		t.Fatal(err)
	}
	b := NewBind()
	dev := device.NewDevice(tunDev, b, device.NewLogger(device.LogLevelSilent, ""))
	n := &Node{Dev: dev, Bind: b, Key: key, Name: name, CheckIn: g.checkIn, LocalOnly: true, Log: t.Logf}
	if err := n.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dev.Close)
	return &testNode{n, tnet}
}

func TestTwoDevicesTalk(t *testing.T) {
	g := &fakeGateway{nodes: map[string]*Peer{}}
	a, b := newTestNode(t, g, "alpha"), newTestNode(t, g, "beta")
	ctx := context.Background()
	for i := 0; i < 2; i++ { // two rounds so each sees the other's endpoints
		if err := a.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		if err := b.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}

	big := make([]byte, 4<<20)
	for i := range big {
		big[i] = byte(i * 7)
	}
	l, err := a.net.ListenTCPAddrPort(netip.MustParseAddrPort(a.Answer.IP + ":80"))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "hello from alpha") })
	mux.HandleFunc("/big", func(w http.ResponseWriter, _ *http.Request) { w.Write(big) })
	go http.Serve(l, mux)

	client := &http.Client{Transport: &http.Transport{DialContext: b.net.DialContext}, Timeout: 20 * time.Second}
	t0 := time.Now()
	r, err := client.Get("http://" + a.Answer.IP + "/")
	if err != nil {
		t.Fatalf("beta can't reach alpha at %s: %v", a.Answer.IP, err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if string(body) != "hello from alpha" {
		t.Fatalf("got %q", body)
	}
	t.Logf("first request %s (includes the WireGuard handshake)", time.Since(t0))

	t1 := time.Now()
	r, err = client.Get("http://" + a.Answer.IP + "/big")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if sha256.Sum256(got) != sha256.Sum256(big) {
		t.Fatalf("4 MB came back different (%d bytes)", len(got))
	}
	t.Logf("4 MB in %s", time.Since(t1))

	for _, s := range b.Status() {
		if s.Name == "alpha" && !s.Connected {
			t.Fatalf("beta's status should show alpha connected: %+v", s)
		}
	}
}
