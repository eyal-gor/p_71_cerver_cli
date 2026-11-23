package cmd

import (
	"strings"
	"testing"
	"time"
)

func TestFocusDomainsAndSchedule(t *testing.T) {
	d := focusDomains([]string{"youtube", "https://News.YCombinator.com/x", "bad domain", "1.2.3.4 evil.com"})
	j := strings.Join(d, " ")
	for _, want := range []string{"youtube.com", "youtu.be", "news.ycombinator.com", "www.news.ycombinator.com"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %s in %s", want, j)
		}
	}
	if strings.Contains(j, "evil") || strings.Contains(j, "bad") {
		t.Errorf("invalid entry kept: %s", j)
	}
	h := &focusHours{From: 9, To: 18, Days: []int{1, 2, 3, 4, 5}}
	mon10 := time.Date(2026, 11, 23, 10, 0, 0, 0, time.Local) // a Monday
	sat10 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.Local)
	mon19 := time.Date(2026, 11, 23, 19, 0, 0, 0, time.Local)
	f := focusSettings{Sites: []string{"x"}, Policy: "work_hours", Hours: h}
	if !focusOn(f, mon10) || focusOn(f, sat10) || focusOn(f, mon19) {
		t.Error("work hours wrong")
	}
	f.Policy = "until"
	f.Until = mon10.Add(time.Hour).Unix()
	if !focusOn(f, mon10) || focusOn(f, mon10.Add(2*time.Hour)) {
		t.Error("until wrong")
	}
	text := "127.0.0.1 localhost\n"
	in := replaceFocusBlock(text, focusBlock([]string{"x.com"}))
	if !strings.Contains(in, "0.0.0.0\tx.com") || replaceFocusBlock(in, "") != text {
		t.Error("block replace wrong")
	}
}
