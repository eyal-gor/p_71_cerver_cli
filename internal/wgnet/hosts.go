package wgnet

import (
	"os"
	"sort"
	"strings"
)

// Names: each device on your network is reachable as `<name>` and
// `<name>.cerver` (e.g. ssh macbook). They live in a marked block of
// /etc/hosts that Cerver owns and rewrites; nothing outside it is touched.

const (
	hostsBegin = "# >>> cerver net (managed; edits here are replaced) >>>"
	hostsEnd   = "# <<< cerver net <<<"
)

// HostName turns a device name into something DNS-safe: "Eyal's MacBook Pro" → "eyals-macbook-pro".
func HostName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r == '\'' || r == '’':
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// HostsBlock renders the managed block for these name→address pairs.
// Two devices with the same name: the first (lowest address) keeps the short name.
func HostsBlock(devices map[string]string) string {
	type d struct{ name, ip string }
	var list []d
	for ip, name := range devices {
		list = append(list, d{HostName(name), ip})
	}
	sort.Slice(list, func(i, j int) bool { return ipLess(list[i].ip, list[j].ip) })
	seen := map[string]bool{}
	var b strings.Builder
	b.WriteString(hostsBegin + "\n")
	for _, x := range list {
		if x.name == "" || seen[x.name] {
			continue
		}
		seen[x.name] = true
		b.WriteString(x.ip + "\t" + x.name + " " + x.name + ".cerver\n")
	}
	b.WriteString(hostsEnd + "\n")
	return b.String()
}

// ReplaceHostsBlock swaps the managed block in hosts-file text (block == "" removes it).
func ReplaceHostsBlock(text, block string) string {
	if i := strings.Index(text, hostsBegin); i >= 0 {
		if j := strings.Index(text[i:], hostsEnd); j >= 0 {
			end := i + j + len(hostsEnd)
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

// WriteHosts updates path (normally /etc/hosts) only when the block changed.
func WriteHosts(path, block string) error {
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next := ReplaceHostsBlock(string(old), block)
	if next == string(old) {
		return nil
	}
	tmp := path + ".cerver-tmp"
	if err := os.WriteFile(tmp, []byte(next), 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func ipLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if len(pa[i]) != len(pb[i]) {
			return len(pa[i]) < len(pb[i])
		}
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}
