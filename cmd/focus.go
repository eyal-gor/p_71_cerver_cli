package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Add-ons → Focus: block distracting sites (Facebook, Instagram, YouTube, …)
// on this whole computer, by a schedule you choose.
//
// The choice lives in ~/.cerver/addons.json under "focus":
//
//   {"sites": ["facebook", "youtube", "news.ycombinator.com"],
//    "policy": "always" | "work_hours" | "until" | "off",
//    "until": 1795400000,              // policy "until": blocked until then
//    "hours": {"from": 9, "to": 18, "days": [1,2,3,4,5]}}   // work_hours
//
// The Cerver network service (root, already running) reads it every few
// seconds and writes the blocked names into a marked block of /etc/hosts,
// so changing it needs no password. Apps that bypass the system resolver
// (a browser's own "secure DNS") may need that setting off.

const (
	focusBegin = "# >>> cerver focus (managed; edits here are replaced) >>>"
	focusEnd   = "# <<< cerver focus <<<"
)

// Site groups people pick by name; anything else is taken as a domain.
var focusGroups = map[string][]string{
	"facebook":  {"facebook.com", "www.facebook.com", "m.facebook.com", "web.facebook.com", "fb.com"},
	"instagram": {"instagram.com", "www.instagram.com"},
	"youtube":   {"youtube.com", "www.youtube.com", "m.youtube.com", "youtu.be"},
	"x":         {"x.com", "www.x.com", "twitter.com", "www.twitter.com", "mobile.twitter.com"},
	"tiktok":    {"tiktok.com", "www.tiktok.com"},
	"reddit":    {"reddit.com", "www.reddit.com", "old.reddit.com"},
	"netflix":   {"netflix.com", "www.netflix.com"},
	"linkedin":  {"linkedin.com", "www.linkedin.com"},
}

type focusHours struct {
	From int   `json:"from"`
	To   int   `json:"to"`
	Days []int `json:"days"` // 0 = Sunday … 6 = Saturday
}

// One time of day in a schedule: from At (HH:MM) on, block or not.
type schedEntry struct {
	At    string `json:"at"`
	Block bool   `json:"block"`
}

// focusSettings is one rule (sites + when). A computer can have several
// (Rules); the old single-rule form (no Rules) still works.
//
//	policy "schedule": Schedule says, every day (or on Days), from which
//	time on the sites are blocked or open, e.g. 08:00 block, 08:55 open, …
//	Before the first entry of the day, the day before's last entry holds.
type focusSettings struct {
	Name     string          `json:"name,omitempty"`
	Sites    []string        `json:"sites"`
	Policy   string          `json:"policy"`
	Until    int64           `json:"until,omitempty"`
	Hours    *focusHours     `json:"hours,omitempty"`
	Schedule []schedEntry    `json:"schedule,omitempty"`
	Days     []int           `json:"days,omitempty"` // schedule days, 0 = Sunday; none = every day
	Rules    []focusSettings `json:"rules,omitempty"`
}

// rules: the computer's Focus rules, each with defaults filled in.
func (f focusSettings) rules() []focusSettings {
	list := f.Rules
	if len(list) == 0 {
		list = []focusSettings{f}
	}
	out := make([]focusSettings, 0, len(list))
	for _, r := range list {
		if r.Policy == "" {
			r.Policy = "off"
		}
		if r.Hours == nil {
			r.Hours = &focusHours{From: 9, To: 18, Days: []int{1, 2, 3, 4, 5}}
		}
		out = append(out, r)
	}
	return out
}

// scheduleOn: what the schedule says at now.
func scheduleOn(entries []schedEntry, days []int, now time.Time) bool {
	if len(days) > 0 {
		ok := false
		for _, d := range days {
			ok = ok || d == int(now.Weekday())
		}
		if !ok {
			return false
		}
	}
	list := append([]schedEntry(nil), entries...)
	sort.Slice(list, func(i, j int) bool { return list[i].At < list[j].At })
	if len(list) == 0 {
		return false
	}
	hm := now.Format("15:04")
	on := list[len(list)-1].Block // before today's first entry: yesterday's last
	for _, e := range list {
		if e.At <= hm {
			on = e.Block
		}
	}
	return on
}

type FocusStatus struct {
	Blocking bool         `json:"blocking"` // in /etc/hosts right now
	Want     bool         `json:"want"`     // what the schedule says now
	Policy   string       `json:"policy"`
	Sites    []string     `json:"sites"`
	Until    int64        `json:"until,omitempty"`
	Hours    *focusHours  `json:"hours,omitempty"`
	Rules    []RuleStatus `json:"rules"`
}

// RuleStatus: one Focus rule as applied here, and whether it blocks now.
type RuleStatus struct {
	Name     string       `json:"name,omitempty"`
	Policy   string       `json:"policy"`
	Sites    []string     `json:"sites"`
	Schedule []schedEntry `json:"schedule,omitempty"`
	On       bool         `json:"on"`
}

func loadFocus() focusSettings {
	var all struct {
		Focus focusSettings `json:"focus"`
	}
	if b, err := os.ReadFile(addonsFile()); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	f := all.Focus
	if f.Policy == "" {
		f.Policy = "off"
	}
	if f.Hours == nil {
		f.Hours = &focusHours{From: 9, To: 18, Days: []int{1, 2, 3, 4, 5}}
	}
	return f
}

// focusOn: does the schedule block right now?
func focusOn(f focusSettings, now time.Time) bool {
	if len(f.Sites) == 0 {
		return false
	}
	switch f.Policy {
	case "always":
		return true
	case "until":
		return now.Unix() < f.Until
	case "schedule":
		return scheduleOn(f.Schedule, f.Days, now)
	case "work_hours":
		h := f.Hours
		day := false
		for _, d := range h.Days {
			if d == int(now.Weekday()) {
				day = true
			}
		}
		return day && now.Hour() >= h.From && now.Hour() < h.To
	}
	return false
}

// validDomain keeps only plain host names: the block can only ever send a
// name to nowhere, never somewhere else.
func validDomain(d string) bool {
	if len(d) < 3 || len(d) > 253 || !strings.Contains(d, ".") || strings.HasPrefix(d, ".") || strings.HasSuffix(d, ".") {
		return false
	}
	for _, r := range d {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func focusDomains(sites []string) []string {
	set := map[string]bool{}
	for _, s := range sites {
		s = strings.ToLower(strings.TrimSpace(s))
		if g, ok := focusGroups[s]; ok {
			for _, d := range g {
				set[d] = true
			}
			continue
		}
		s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
		s = strings.SplitN(s, "/", 2)[0]
		if validDomain(s) {
			set[s] = true
			if !strings.HasPrefix(s, "www.") {
				set["www."+s] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func focusBlock(domains []string) string {
	if len(domains) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(focusBegin + "\n")
	for _, d := range domains {
		b.WriteString("0.0.0.0\t" + d + "\n")
		b.WriteString("::\t" + d + "\n")
	}
	b.WriteString(focusEnd + "\n")
	return b.String()
}

func replaceFocusBlock(text, block string) string {
	if i := strings.Index(text, focusBegin); i >= 0 {
		if j := strings.Index(text[i:], focusEnd); j >= 0 {
			end := i + j + len(focusEnd)
			if end < len(text) && text[end] == '\n' {
				end++
			}
			text = text[:i] + text[end:]
		}
	}
	if block == "" {
		return text
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	return text + block
}

const hostsPath = "/etc/hosts"

// applyFocus brings /etc/hosts in line with the schedule (root only; the
// network service calls it every few seconds). Writes only on a change.
func applyFocus() {
	if os.Geteuid() != 0 {
		return
	}
	f := loadFocus()
	var sites []string
	for _, r := range f.rules() {
		if focusOn(r, time.Now()) {
			sites = append(sites, r.Sites...)
		}
	}
	block := focusBlock(focusDomains(sites))
	old, err := os.ReadFile(hostsPath)
	if err != nil {
		return
	}
	next := replaceFocusBlock(string(old), block)
	if next == string(old) {
		return
	}
	tmp := hostsPath + ".cerver-focus-tmp"
	if os.WriteFile(tmp, []byte(next), 0o644) != nil || os.Rename(tmp, hostsPath) != nil {
		return
	}
	_ = exec.Command("/usr/bin/dscacheutil", "-flushcache").Run()
	_ = exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder").Run()
}

func focusStatus() FocusStatus {
	f := loadFocus()
	b, _ := os.ReadFile(hostsPath)
	sites := f.Sites
	if sites == nil {
		sites = []string{}
	}
	want := false
	rules := []RuleStatus{}
	for _, r := range f.rules() {
		on := focusOn(r, time.Now())
		want = want || on
		rs := r.Sites
		if rs == nil {
			rs = []string{}
		}
		rules = append(rules, RuleStatus{Name: r.Name, Policy: r.Policy, Sites: rs, Schedule: r.Schedule, On: on})
	}
	return FocusStatus{
		Rules:    rules,
		Blocking: strings.Contains(string(b), focusBegin),
		Want:     want,
		Policy:   f.Policy,
		Sites:    sites,
		Until:    f.Until,
		Hours:    f.Hours,
	}
}

// ── Add-ons chosen from the app for this device (any computer on the network)

var lastRemoteAddons string

// remoteAddons applies the add-ons the owner chose for this device in the
// app (handed out by the gateway at check-in). Only a change is applied; with
// none chosen remotely, the local settings stay as they are.
func remoteAddons(raw json.RawMessage) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == lastRemoteAddons || os.Geteuid() != 0 {
		return
	}
	var want struct {
		Level string          `json:"level"`
		Focus json.RawMessage `json:"focus"`
	}
	if json.Unmarshal(raw, &want) != nil {
		return
	}
	// Focus: into the settings file; applyFocus (every few seconds) does the rest.
	f := addonsFile()
	all := map[string]any{}
	if b, err := os.ReadFile(f); err == nil {
		_ = json.Unmarshal(b, &all)
	}
	if len(want.Focus) > 0 {
		var fo any
		if json.Unmarshal(want.Focus, &fo) == nil {
			all["focus"] = fo
		}
	}
	if b, err := json.Marshal(all); err == nil {
		_ = os.WriteFile(f, b, 0o644)
	}
	// Safety DNS: only when it differs from what's in effect.
	if want.Level != "" && getAddons().Level != want.Level {
		if err := setAddons(want.Level); err != nil {
			fmt.Printf("Couldn't apply add-ons (%s): %v\n", want.Level, err)
			return // try again at the next check-in
		}
	}
	applyFocus()
	lastRemoteAddons = s
}

// addonsApplied: what's in effect here, for the owner's app.
func addonsApplied() json.RawMessage {
	st := getAddons()
	b, _ := json.Marshal(map[string]any{
		"level": st.Level, "dns": st.DNS,
		"focus": map[string]any{"policy": st.Focus.Policy, "sites": st.Focus.Sites, "blocking": st.Focus.Blocking, "until": st.Focus.Until, "rules": st.Focus.Rules},
		"exit":  exitApplied(),
		"at":    time.Now().Unix(),
	})
	return b
}
