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

func TestFocusSchedule(t *testing.T) {
	s := []schedEntry{{"08:00", true}, {"08:55", false}, {"09:00", true}, {"17:55", false}}
	at := func(h, m int) time.Time { return time.Date(2026, 11, 23, h, m, 0, 0, time.Local) }
	cases := map[[2]int]bool{{7, 0}: false, {8, 0}: true, {8, 30}: true, {8, 56}: false, {9, 1}: true, {17, 54}: true, {18, 0}: false, {23, 59}: false}
	for hm, want := range cases {
		if got := scheduleOn(s, nil, at(hm[0], hm[1])); got != want {
			t.Errorf("%02d:%02d: got %v want %v", hm[0], hm[1], got, want)
		}
	}
	// Overnight: block from 22:00 until 07:00.
	n := []schedEntry{{"07:00", false}, {"22:00", true}}
	if !scheduleOn(n, nil, at(3, 0)) || scheduleOn(n, nil, at(12, 0)) || !scheduleOn(n, nil, at(23, 0)) {
		t.Error("overnight wrong")
	}
	// Rules: union of what's on.
	f := focusSettings{Rules: []focusSettings{{Sites: []string{"x"}, Policy: "always"}, {Sites: []string{"lichess.org"}, Policy: "off"}}}
	on := 0
	for _, r := range f.rules() {
		if focusOn(r, at(10, 0)) {
			on++
		}
	}
	if on != 1 {
		t.Errorf("rules on = %d", on)
	}
}
