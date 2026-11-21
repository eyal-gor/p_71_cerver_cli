package wgnet

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// fakeRelay does what the gateway's relay does: route by public key.
type fakeRelay struct {
	mu    sync.Mutex
	binds map[[32]byte]*Bind
	n     int
}

type relayPort struct {
	r    *fakeRelay
	self [32]byte
}

func (p relayPort) SendTo(to [32]byte, pkt []byte) error {
	p.r.mu.Lock()
	b := p.r.binds[to]
	p.r.n++
	p.r.mu.Unlock()
	if b != nil {
		b.FromRelay(p.self, append([]byte(nil), pkt...))
	}
	return nil
}

// TestBlockedDevicesUseTheRelay: each device's only direct address goes
// nowhere (like a strict office network), so they must fall back to the relay.
func TestBlockedDevicesUseTheRelay(t *testing.T) {
	g := &fakeGateway{nodes: map[string]*Peer{}}
	relay := &fakeRelay{binds: map[[32]byte]*Bind{}}
	mk := func(name string) *testNode {
		key, _ := NewKey()
		ans, _ := g.checkIn(context.Background(), CheckInRequest{PublicKey: key.PublicB64(), Name: name, Endpoints: []string{"192.0.2.1:9"}})
		tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(ans.IP)}, nil, 1280)
		if err != nil {
			t.Fatal(err)
		}
		b := NewBind()
		relay.binds[key.Public] = b
		b.SetRelay(relayPort{relay, key.Public})
		dev := device.NewDevice(tunDev, b, device.NewLogger(device.LogLevelSilent, ""))
		blocked := func(ctx context.Context, r CheckInRequest) (*CheckInAnswer, error) {
			r.Endpoints = []string{"192.0.2.1:9"} // TEST-NET: nothing answers there
			return g.checkIn(ctx, r)
		}
		n := &Node{Dev: dev, Bind: b, Key: key, Name: name, CheckIn: blocked, LocalOnly: true, Relay: true, TryFor: 6 * time.Second, Log: t.Logf}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dev.Close)
		return &testNode{n, tnet}
	}
	a, b := mk("office"), mk("home")
	ctx := context.Background()
	a.Tick(ctx)
	b.Tick(ctx)
	a.Tick(ctx)
	b.Tick(ctx)

	for _, s := range b.Status() {
		t.Logf("before: home→%s via %s connected=%v", s.Name, s.Endpoint, s.Connected)
	}
	l, err := a.net.ListenTCPAddrPort(netip.MustParseAddrPort(a.Answer.IP + ":80"))
	if err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 1<<20)
	for i := range big {
		big[i] = byte(i * 13)
	}
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(big) }))

	// Keep moving on to the next address, as the real command does every few seconds.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
				a.Retry()
				b.Retry()
			}
		}
	}()

	client := &http.Client{Transport: &http.Transport{DialContext: b.net.DialContext}, Timeout: 30 * time.Second}
	t0 := time.Now()
	r, err := client.Get("http://" + a.Answer.IP + "/")
	if err != nil {
		t.Fatalf("home can't reach office through the relay: %v", err)
	}
	got, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if sha256.Sum256(got) != sha256.Sum256(big) {
		t.Fatalf("1 MB came back different (%d bytes)", len(got))
	}
	t.Logf("1 MB through the relay; found the way in %s (the dead address gets 6 s); %d frames relayed", time.Since(t0), relay.n)
	for _, s := range b.Status() {
		if s.Endpoint != "Cerver relay" || !s.Connected {
			t.Fatalf("status should say connected via Cerver relay: %+v", s)
		}
	}
	_ = base64.StdEncoding
}
