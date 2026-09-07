// Package feed discovers the current Grok Bot upstream release.
//
// Grok Bot is published by Cursor (xAI) but the desktop update service
// registers the app under the name "sand", not "grok-bot". The Linux feeds
// are the only ones that carry the content hash (build ID) needed to
// reconstruct the .deb URLs:
//
//	https://api2.cursor.sh/updates/api/update/linux-x64/sand/0.0.0/stable
//	https://api2.cursor.sh/updates/api/update/linux-arm64/sand/0.0.0/stable
//
// A Linux response looks like:
//
//	{"version":"0.44.0",
//	 "url":"https://downloads.cursor.com/grokbot/stable/<40-hex>/linux/x64/Grok_Bot_0.44.0.AppImage",
//	 "productVersion":"0.44.0", ...}
//
// The darwin/win32 feeds expose only the version (their URLs have no hash),
// so they are used as a version fallback. As a last resort the scraper reads
// https://cursor.com/download/bot and extracts the embedded
// downloads.cursor.com/grokbot/stable/... URLs.
package feed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Endpoints queried in order. Linux feeds carry the build ID; the others
// only carry the version and are used as fallback.
var (
	LinuxX64Feed   = "https://api2.cursor.sh/updates/api/update/linux-x64/sand/0.0.0/stable"
	LinuxArm64Feed = "https://api2.cursor.sh/updates/api/update/linux-arm64/sand/0.0.0/stable"
	// Version-only fallbacks (no build hash in their URLs).
	DarwinArm64Feed = "https://api2.cursor.sh/updates/api/update/darwin-arm64/sand/0.0.0/stable"
	DarwinX64Feed   = "https://api2.cursor.sh/updates/api/update/darwin-x64/sand/0.0.0/stable"
	WinX64Feed      = "https://api2.cursor.sh/updates/api/update/win32-x64/sand/0.0.0/stable"

	DownloadPage = "https://cursor.com/download/bot"
)

// Release is one upstream Grok Bot release.
type Release struct {
	Version      string `json:"version"`
	BuildID      string `json:"build_id"`      // 40-hex content hash
	DownloadBase string `json:"download_base"` // https://downloads.cursor.com/grokbot/stable
	DebAMD64     string `json:"deb_amd64"`
	DebARM64     string `json:"deb_arm64"`
	// Raw feed URL that produced the build ID (AppImage URL).
	SourceURL string `json:"source_url"`
}

var (
	// https://downloads.cursor.com/grokbot/stable/<hash>/linux/x64/...
	hashURLRe = regexp.MustCompile(`^(https://downloads\.cursor\.com/[^/]+/stable)/([0-9a-f]{40})/`)
	// Fallback scraper: any stable linux deb URL on the download page.
	scrapeDebRe = regexp.MustCompile(`https://downloads\.cursor\.com/grokbot/stable/([0-9a-f]{40})/linux/(x64|arm64)/(grok-bot_[0-9][^"'\\\s]*?_(?:amd64|arm64)\.deb)`)
	scrapeVerRe = regexp.MustCompile(`grok-bot_([0-9]+\.[0-9]+\.[0-9]+[^_]*?)_(?:amd64|arm64)\.deb`)
)

type feedDoc struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	ProductVersion string `json:"productVersion"`
	URL            string `json:"url"`
}

func versionOf(d feedDoc) string {
	for _, s := range []string{d.Name, d.Version, d.ProductVersion} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// Client is a small wrapper so callers can inject timeouts / test servers.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (c *Client) getJSON(url string) (feedDoc, error) {
	var d feedDoc
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return d, err
	}
	req.Header.Set("User-Agent", "grok-rpm/1.0 (+https://github.com/addidotlol/grok-rpm)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return d, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return d, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return d, fmt.Errorf("GET %s: invalid JSON: %w", url, err)
	}
	return d, nil
}

// Latest queries the Linux feeds (which carry the build hash) and returns
// the release. If only version-only feeds answer, it returns a Release with
// an empty BuildID and the caller should resolve the hash via the download
// page (see LatestWithFallback).
func (c *Client) Latest() (Release, error) {
	for _, feedURL := range []string{LinuxX64Feed, LinuxArm64Feed} {
		d, err := c.getJSON(feedURL)
		if err != nil {
			continue
		}
		if rel, ok := fromFeedDoc(d); ok {
			return rel, nil
		}
	}
	// Fallback: version from darwin/win feeds + hash from download page.
	var version string
	for _, feedURL := range []string{DarwinArm64Feed, DarwinX64Feed, WinX64Feed} {
		if d, err := c.getJSON(feedURL); err == nil {
			if v := versionOf(d); v != "" {
				version = v
				break
			}
		}
	}
	if version == "" {
		// Last resort: scrape the download page for both version and hash.
		return c.scrapeDownloadPage()
	}
	rel, err := c.scrapeDownloadPage()
	if err != nil {
		// Return version without URLs so callers can report clearly.
		return Release{Version: version}, fmt.Errorf("version %s known but build hash unavailable: %w", version, err)
	}
	_ = version
	return rel, nil
}

func fromFeedDoc(d feedDoc) (Release, bool) {
	version := versionOf(d)
	if version == "" || d.URL == "" {
		return Release{}, false
	}
	m := hashURLRe.FindStringSubmatch(d.URL)
	if m == nil {
		return Release{}, false
	}
	base, build := m[1], m[2]
	return Release{
		Version:      version,
		BuildID:      build,
		DownloadBase: base,
		DebAMD64:     fmt.Sprintf("%s/%s/linux/x64/grok-bot_%s_amd64.deb", base, build, version),
		DebARM64:     fmt.Sprintf("%s/%s/linux/arm64/grok-bot_%s_arm64.deb", base, build, version),
		SourceURL:    d.URL,
	}, true
}

// scrapeDownloadPage parses https://cursor.com/download/bot for linux deb URLs.
func (c *Client) scrapeDownloadPage() (Release, error) {
	req, err := http.NewRequest("GET", DownloadPage, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("User-Agent", "grok-rpm/1.0 (+https://github.com/addidotlol/grok-rpm)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return Release{}, fmt.Errorf("GET %s: HTTP %d", DownloadPage, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Release{}, err
	}
	return ParseDownloadPage(string(body))
}

// ParseDownloadPage extracts the release from download-page HTML.
func ParseDownloadPage(html string) (Release, error) {
	matches := scrapeDebRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return Release{}, fmt.Errorf("no grokbot linux .deb URLs found on download page")
	}
	// All matches should share one hash; prefer the first x64 hit for version.
	var build, version string
	var amd64, arm64 string
	for _, m := range matches {
		h, arch, file := m[1], m[2], m[3]
		if build == "" {
			build = h
		}
		if h != build {
			continue // ignore mixed-hash pages; first hash wins
		}
		url := "https://downloads.cursor.com/grokbot/stable/" + h + "/linux/" + arch + "/" + file
		switch arch {
		case "x64":
			if amd64 == "" {
				amd64 = url
				if vm := scrapeVerRe.FindStringSubmatch(file); vm != nil {
					version = vm[1]
				}
			}
		case "arm64":
			if arm64 == "" {
				arm64 = url
				if version == "" {
					if vm := scrapeVerRe.FindStringSubmatch(file); vm != nil {
						version = vm[1]
					}
				}
			}
		}
	}
	if build == "" || version == "" {
		return Release{}, fmt.Errorf("could not determine version/build from download page")
	}
	return Release{
		Version:      version,
		BuildID:      build,
		DownloadBase: "https://downloads.cursor.com/grokbot/stable",
		DebAMD64:     amd64,
		DebARM64:     arm64,
		SourceURL:    DownloadPage,
	}, nil
}

// DebURL returns the .deb URL for a Debian arch name.
func (r Release) DebURL(debianArch string) string {
	switch debianArch {
	case "arm64":
		return r.DebARM64
	default:
		return r.DebAMD64
	}
}
