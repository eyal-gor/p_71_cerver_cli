package wgnet

import (
	"net/netip"
	"sync/atomic"

	"golang.zx2c4.com/wireguard/tun"
)

// SelfTUN delivers packets this device sends to its own network address.
// On macOS such packets go into the tunnel, and WireGuard drops them (there is
// no peer for our own address), so `ping macbook` from the MacBook failed.
// We catch them on the way in and write them straight back out of the
// interface, where the system receives them as local traffic.
//
// It is also where this device's access rules apply (Filter): packets going
// out are remembered so their replies get back in, and packets coming in
// from the tunnel are dropped unless the rules allow them.
type SelfTUN struct {
	tun.Device
	self   atomic.Value // [4]byte; zero until the address is known
	Filter *Filter
}

func NewSelfTUN(d tun.Device) *SelfTUN { return &SelfTUN{Device: d} }

// SetSelf records this device's address (e.g. "100.96.0.2").
func (t *SelfTUN) SetSelf(ip string) {
	if a, err := netip.ParseAddr(ip); err == nil && a.Is4() {
		t.self.Store(a.As4())
	}
}

func (t *SelfTUN) Read(bufs [][]byte, sizes []int, offset int) (int, error) {
	for {
		n, err := t.Device.Read(bufs, sizes, offset)
		if n == 0 || err != nil {
			return n, err
		}
		self, ok := t.self.Load().([4]byte)
		if !ok {
			return n, nil
		}
		kept := 0
		for i := 0; i < n; i++ {
			p := bufs[i][offset : offset+sizes[i]]
			if len(p) >= 20 && p[0]>>4 == 4 && [4]byte(p[16:20]) == self {
				t.Device.Write([][]byte{bufs[i][:offset+sizes[i]]}, offset)
				continue
			}
			if t.Filter != nil {
				t.Filter.Outbound(p)
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

// Write delivers packets from the tunnel to this device, minus the ones its
// access rules refuse.
func (t *SelfTUN) Write(bufs [][]byte, offset int) (int, error) {
	if t.Filter == nil || t.Filter.Open() {
		return t.Device.Write(bufs, offset)
	}
	kept := make([][]byte, 0, len(bufs))
	for _, b := range bufs {
		if len(b) > offset && t.Filter.Inbound(b[offset:]) {
			kept = append(kept, b)
		}
	}
	if len(kept) == 0 {
		return len(bufs), nil
	}
	if _, err := t.Device.Write(kept, offset); err != nil {
		return 0, err
	}
	return len(bufs), nil
}
