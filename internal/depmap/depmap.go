// Package depmap translates Debian dependency names to Fedora/RHEL
// (dnf) RPM capability names.
//
// Debian and Fedora name the same system libraries differently:
// libgtk-3-0 (deb) vs gtk3 (rpm), libnss3 vs nss, and so on. This table
// covers the libraries Electron apps such as Grok Bot actually need. Names
// not in the table fall back to the raw Debian name so the generated spec
// still shows what is missing; Map() reports which entries were unmapped.
package depmap

import (
	"strings"

	"github.com/addidotlol/grok-rpm/internal/deb"
)

// debianToRPM maps lower-cased Debian package names to RPM capabilities.
var debianToRPM = map[string]string{
	// Grok Bot 0.44.0 direct deps (from .deb control).
	"libgtk-3-0":         "gtk3",
	"libnotify4":         "libnotify",
	"libnss3":            "nss",
	"libxss1":            "libXScrnSaver",
	"libxtst6":           "libXtst",
	"xdg-utils":          "xdg-utils",
	"libatspi2.0-0":      "at-spi2-core",
	"libuuid1":           "libuuid",
	"libsecret-1-0":      "libsecret",
	"libasound2t64":      "alsa-lib",
	"libasound2":         "alsa-lib",
	"libgbm1":            "mesa-libgbm",
	"libxkbcommon0":      "libxkbcommon",
	"libdrm2":            "libdrm",
	"libappindicator3-1": "libappindicator-gtk3",
	"libappindicator1":   "libappindicator-gtk3",

	// Common Electron transitive deps (present on most Cursor-family debs).
	"libatk1.0-0":         "atk",
	"libatk-bridge2.0-0":  "at-spi2-atk",
	"libatspi2.0-dev":     "at-spi2-core",
	"libcairo2":           "cairo",
	"libcups2":            "cups-libs",
	"libcups2t64":         "cups-libs",
	"libdbus-1-3":         "dbus-libs",
	"libexpat1":           "expat",
	"libgtk-3-0t64":       "gtk3",
	"libnspr4":            "nspr",
	"libnss3t64":          "nss",
	"libpango-1.0-0":      "pango",
	"libpangocairo-1.0-0": "pangocairo",
	"libx11-6":            "libX11",
	"libxcb1":             "libxcb",
	"libxcomposite1":      "libXcomposite",
	"libxcursor1":         "libXcursor",
	"libxdamage1":         "libXdamage",
	"libxext6":            "libXext",
	"libxfixes3":          "libXfixes",
	"libxi6":              "libXi",
	"libxrandr2":          "libXrandr",
	"libxrender1":         "libXrender",
	"libxtst6t64":         "libXtst",
	"libxss1t64":          "libXScrnSaver",
	"libasound2-dev":      "alsa-lib",
	"libpulse0":           "pulseaudio-libs",
	"libegl1":             "mesa-libEGL",
	"libgl1":              "mesa-libGL",
	"libgles2":            "mesa-libGLES",
	"libvulkan1":          "vulkan-loader",
	"libatomic1":          "libatomic",
	"libwayland-client0":  "libwayland-client",
	"libwayland-egl1":     "mesa-libwayland-egl",
	"libcurl4":            "libcurl",
	"libssl3":             "openssl-libs",
	"ca-certificates":     "ca-certificates",
	"hicolor-icon-theme":  "hicolor-icon-theme",
	"desktop-file-utils":  "desktop-file-utils",
	"libnotify-bin":       "libnotify",
	"libsecret-common":    "libsecret",
	"libuuid-runtime":     "util-linux",
	"libgbm-dev":          "mesa-libgbm",
	"fonts-liberation":    "liberation-fonts",
}

// Map converts one raw Debian dependency token (which may contain "|"
// alternatives) to an RPM requirement. For "a | b" it returns the mapping
// of the first alternative that has a known mapping; if none is known it
// returns the first alternative's raw name and mapped=false.
func Map(raw string) (rpm string, mapped bool) {
	alts := strings.Split(raw, "|")
	for _, a := range alts {
		name := deb.DepName(a)
		if name == "" {
			continue
		}
		if rpm, ok := debianToRPM[strings.ToLower(name)]; ok {
			return rpm, true
		}
	}
	// No known mapping: fall back to the first alternative's bare name.
	first := ""
	if len(alts) > 0 {
		first = deb.DepName(alts[0])
	}
	if first == "" {
		first = strings.TrimSpace(raw)
	}
	return first, false
}

// MapList converts a Debian dependency list. Duplicates are removed,
// order is preserved. It returns the RPM names plus the subset of raw
// Debian tokens that had no known mapping (for warnings).
func MapList(deps []string) (rpms []string, unmapped []string) {
	seen := map[string]bool{}
	for _, d := range deps {
		rpm, ok := Map(d)
		if rpm == "" {
			continue
		}
		if !ok {
			unmapped = append(unmapped, d)
		}
		if !seen[rpm] {
			seen[rpm] = true
			rpms = append(rpms, rpm)
		}
	}
	return rpms, unmapped
}
