package wgnet

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// TestBackToDirect: two devices that ended up on the backup relay (say the
// home Wi-Fi blinked) move back to the direct path once it answers again,
// and traffic keeps flowing.
func TestBackToDirect(t *testing.T) {
	g := &fakeGateway{nodes: map[string]*Peer{}}
	relay := &fakeRelay{binds: map[[32]byte]*Bind{}}
	mk := func(name string) *testNode {
		key, _ := NewKey()
		ans, _ := g.checkIn(context.Background(), CheckInRequest{PublicKey: key.PublicB64(), Name: name})
		tunDev, tnet, err := netstack.CreateNetTUN([]netip.Addr{netip.MustParseAddr(ans.IP)}, nil, 1280)
		if err != nil {
			t.Fatal(err)
		}
		b := NewBind()
		relay.binds[key.Public] = b
		b.SetRelay(relayPort{relay, key.Public})
		dev := device.NewDevice(tunDev, b, device.NewLogger(device.LogLevelSilent, ""))
		n := &Node{Dev: dev, Bind: b, Key: key, Name: name, CheckIn: g.checkIn, LocalOnly: true, Relay: true, TryFor: 6 * time.Second, ProbeEvery: time.Second, Log: t.Logf}
		if err := n.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dev.Close)
		return &testNode{n, tnet}
	}
	a, b := mk("mini"), mk("air")
	ctx := context.Background()
	a.Tick(ctx)
	b.Tick(ctx)
	a.Tick(ctx)
	b.Tick(ctx)

	// Put both on the relay, as after a blip.
	onRelay := func(n, other *testNode) {
		hexKey := hex.EncodeToString(other.Key.Public[:])
		if err := n.Dev.IpcSet("public_key=" + hexKey + "\nendpoint=" + RelayAddr(other.Key.Public) + "\n"); err != nil {
			t.Fatal(err)
		}
		n.mu.Lock()
		st := n.peers[hexKey]
		st.current, st.setAt = RelayAddr(other.Key.Public), time.Now()
		n.mu.Unlock()
	}
	onRelay(a, b)
	onRelay(b, a)

	l, err := a.net.ListenTCPAddrPort(netip.MustParseAddrPort(a.Answer.IP + ":80"))
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(l, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) }))
	client := &http.Client{Transport: &http.Transport{DialContext: b.net.DialContext, DisableKeepAlives: true}, Timeout: 20 * time.Second}
	get := func() {
		r, err := client.Get("http://" + a.Answer.IP + "/")
		if err != nil {
			t.Fatalf("air can't reach mini: %v", err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if string(body) != "ok" {
			t.Fatalf("got %q", body)
		}
	}
	get()
	via := func(n *testNode) string { return n.Status()[0].Endpoint }
	if via(a) != "Cerver relay" || via(b) != "Cerver relay" {
		t.Fatalf("setup: expected both on the relay, got %s / %s", via(a), via(b))
	}
	relayed := relay.n

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		a.Retry()
		b.Retry()
		get()
		if !strings.HasPrefix(via(a), "Cerver") && !strings.HasPrefix(via(b), "Cerver") {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if strings.HasPrefix(via(a), "Cerver") || strings.HasPrefix(via(b), "Cerver") {
		t.Fatalf("still on the relay: mini→%s air→%s", via(a), via(b))
	}
	// Traffic now goes direct: the relay carries nothing more.
	before := relay.n
	for i := 0; i < 5; i++ {
		get()
	}
	t.Logf("back to direct: mini→%s air→%s; relay frames during setup %d, after %d", via(a), via(b), relayed, relay.n-before)
	if relay.n-before > 2 {
		t.Fatalf("traffic still going through the relay (%d frames)", relay.n-before)
	}
}
