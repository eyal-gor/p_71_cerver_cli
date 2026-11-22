package cmd

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Releases live on GitHub (eyal-gor/p_71_cerver_cli). Each one carries
//
//	cerver-darwin-arm64, cerver-darwin-amd64, cerver-linux-amd64, cerver-linux-arm64
//	Cerver.zip   (the Mac app)
//	SHA256SUMS   (every file above)
//
// and its tag is the version: v1.0.N. Nothing is installed unless its
// checksum matches SHA256SUMS from the same release.
const releaseRepo = "https://github.com/eyal-gor/p_71_cerver_cli"

var httpc = &http.Client{Timeout: 5 * time.Minute}

// latestRelease returns the newest release's version ("1.0.42"), read from
// where github.com/…/releases/latest redirects (no API rate limit).
func latestRelease(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "HEAD", releaseRepo+"/releases/latest", nil)
	c := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	i := strings.LastIndex(loc, "/tag/v")
	if i < 0 {
		return "", fmt.Errorf("no release published yet")
	}
	return loc[i+len("/tag/v"):], nil
}

// releaseAsset downloads one file of release v and checks it against that
// release's SHA256SUMS.
func releaseAsset(ctx context.Context, v, name string) ([]byte, error) {
	base := releaseRepo + "/releases/download/v" + v + "/"
	sums, err := fetch(ctx, base+"SHA256SUMS")
	if err != nil {
		return nil, err
	}
	want := ""
	sc := bufio.NewScanner(strings.NewReader(string(sums)))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("release v%s has no %s", v, name)
	}
	data, err := fetch(ctx, base+name)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s from release v%s failed its checksum; not installing it", name, v)
	}
	return data, nil
}

func fetch(ctx context.Context, url string) ([]byte, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 200<<20))
}

func cliAssetName() string { return "cerver-" + runtime.GOOS + "-" + runtime.GOARCH }

// replaceFile swaps in new contents atomically (same folder, then rename), so
// a running copy keeps working and the next start gets the new one.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cerver-new-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// The relay's version is its repo's commit count (it prints "v496" when it
// starts). Latest = commits on main; running = the last start in its log.

var relayBanner = regexp.MustCompile(`Cerver Local Compute\S*\s+v(\d+)`)

func relayRunningVersion() string {
	b, err := os.ReadFile(relayLog())
	if err != nil {
		return ""
	}
	m := relayBanner.FindAllSubmatch(b, -1)
	if len(m) == 0 {
		return ""
	}
	return string(m[len(m)-1][1])
}

var lastPage = regexp.MustCompile(`[?&]page=(\d+)>;\s*rel="last"`)

func relayLatestVersion(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com/repos/eyal-gor/p_69_cerver_relay/commits?sha=main&per_page=1", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GitHub: %s", resp.Status)
	}
	m := lastPage.FindStringSubmatch(resp.Header.Get("Link"))
	if m == nil {
		return "", fmt.Errorf("couldn't read the relay's latest version")
	}
	return m[1], nil
}
