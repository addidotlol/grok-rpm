package depmap

import "testing"

func TestMapKnown(t *testing.T) {
	for deb, want := range map[string]string{
		"libgtk-3-0":            "gtk3",
		"libnss3":               "nss",
		"libxss1":               "libXScrnSaver",
		"libasound2t64":         "alsa-lib",
		"libappindicator3-1":    "libappindicator-gtk3",
		"libfoo (>= 1.2)":       "libfoo", // unknown -> raw fallback
	} {
		got, _ := Map(deb)
		if got != want {
			t.Errorf("Map(%q)=%q want %q", deb, got, want)
		}
	}
}

func TestMapAlternative(t *testing.T) {
	got, ok := Map("libasound2t64 | libasound2")
	if got != "alsa-lib" || !ok {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestMapListDedupes(t *testing.T) {
	rpms, _ := MapList([]string{"libgtk-3-0", "libnotify4", "libnotify4"})
	if len(rpms) != 2 {
		t.Fatalf("rpms=%q", rpms)
	}
}

func TestRealDepList(t *testing.T) {
	deps := []string{
		"libgtk-3-0", "libnotify4", "libnss3", "libxss1", "libxtst6",
		"xdg-utils", "libatspi2.0-0", "libuuid1", "libsecret-1-0",
		"libasound2t64 | libasound2", "libgbm1", "libxkbcommon0", "libdrm2",
	}
	rpms, unmapped := MapList(deps)
	if len(unmapped) != 0 {
		t.Fatalf("unmapped: %q", unmapped)
	}
	if len(rpms) != len(deps) {
		t.Fatalf("rpms=%q", rpms)
	}
}
