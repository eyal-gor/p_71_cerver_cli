package wgnet

import (
	"os"
	"testing"

	"golang.zx2c4.com/wireguard/tun"
)

// fakeTUN hands out queued packets and records what is written back.
type fakeTUN struct {
	in      [][]byte
	written [][]byte
}

func (f *fakeTUN) File() *os.File           { return nil }
func (f *fakeTUN) MTU() (int, error)        { return 1280, nil }
func (f *fakeTUN) Name() (string, error)    { return "fake", nil }
func (f *fakeTUN) Events() <-chan tun.Event { return nil }
func (f *fakeTUN) Close() error             { return nil }
func (f *fakeTUN) BatchSize() int           { return 4 }
func (f *fakeTUN) Write(b [][]byte, off int) (int, error) {
	for _, x := range b {
		f.written = append(f.written, append([]byte(nil), x[off:]...))
	}
	return len(b), nil
}
func (f *fakeTUN) Read(bufs [][]byte, sizes []int, off int) (int, error) {
	n := 0
	for n < len(bufs) && len(f.in) > 0 {
		sizes[n] = copy(bufs[n][off:], f.in[0])
		f.in = f.in[1:]
		n++
	}
	return n, nil
}

func pkt(dst [4]byte, tag byte) []byte {
	p := make([]byte, 20)
	p[0] = 0x45
	copy(p[12:16], []byte{100, 96, 0, 2})
	copy(p[16:20], dst[:])
	p[1] = tag
	return p
}

func TestSelfTUNDeliversOwnTraffic(t *testing.T) {
	me, peer := [4]byte{100, 96, 0, 2}, [4]byte{100, 96, 0, 1}
	f := &fakeTUN{in: [][]byte{pkt(me, 1), pkt(peer, 2), pkt(me, 3), pkt(peer, 4)}}
	s := NewSelfTUN(f)
	s.SetSelf("100.96.0.2")

	bufs := make([][]byte, 4)
	for i := range bufs {
		bufs[i] = make([]byte, 100)
	}
	sizes := make([]int, 4)
	n, _ := s.Read(bufs, sizes, 16)
	if n != 2 || bufs[0][16+1] != 2 || bufs[1][16+1] != 4 {
		t.Fatalf("tunnel should get only the 2 peer packets, got n=%d", n)
	}
	if len(f.written) != 2 || f.written[0][1] != 1 || f.written[1][1] != 3 {
		t.Fatalf("own packets should be handed back, got %d", len(f.written))
	}
}
