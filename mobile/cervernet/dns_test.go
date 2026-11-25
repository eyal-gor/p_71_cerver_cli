package cervernet

import (
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func ask(t *testing.T, d *dnsTUN, name string, typ dnsmessage.Type) (dnsmessage.RCode, []netip.Addr) {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	resp := d.resolve(q)
	var p dnsmessage.Parser
	h, err := p.Start(resp)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	p.SkipAllQuestions()
	var out []netip.Addr
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if rh.Type == dnsmessage.TypeA {
			a, _ := p.AResource()
			out = append(out, netip.AddrFrom4(a.A))
		} else {
			p.SkipAnswer()
		}
	}
	if h.ID != 7 {
		t.Fatalf("id %d", h.ID)
	}
	return h.RCode, out
}

func TestNames(t *testing.T) {
	nm := &names{}
	nm.set(map[string]netip.Addr{"mac-mini": netip.MustParseAddr("100.96.0.1"), "eyals-mac-mini": netip.MustParseAddr("100.96.0.1")})
	d := &dnsTUN{names: nm}
	for _, n := range []string{"mac-mini.cerver.", "Eyals-Mac-Mini.cerver.", "mac-mini."} {
		rc, a := ask(t, d, n, dnsmessage.TypeA)
		if rc != dnsmessage.RCodeSuccess || len(a) != 1 || a[0].String() != "100.96.0.1" {
			t.Fatalf("%s: %v %v", n, rc, a)
		}
	}
	if rc, a := ask(t, d, "mac-mini.cerver.", dnsmessage.TypeAAAA); rc != dnsmessage.RCodeSuccess || len(a) != 0 {
		t.Fatalf("AAAA: %v %v", rc, a)
	}
	if rc, _ := ask(t, d, "nope.cerver.", dnsmessage.TypeA); rc != dnsmessage.RCodeNameError {
		t.Fatalf("unknown: %v", rc)
	}
	if _, _, ours := nm.lookup("example.com."); ours {
		t.Fatal("internet names are not ours")
	}
}

func TestDNSQueryPacket(t *testing.T) {
	p := make([]byte, 40)
	p[0], p[9] = 0x45, 17
	copy(p[16:20], []byte{100, 100, 100, 100})
	p[22], p[23] = 0, 53
	if _, ok := dnsQuery(p); !ok {
		t.Fatal("should be a lookup")
	}
	p[19] = 99
	if _, ok := dnsQuery(p); ok {
		t.Fatal("other address is not a lookup")
	}
	if checksum([]byte{0x45, 0, 0, 0x1c, 0, 0, 0, 0, 64, 17, 0, 0, 100, 100, 100, 100, 100, 96, 0, 3}) == 0 {
		t.Fatal("checksum")
	}
}
