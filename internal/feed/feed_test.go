package feed

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFromLinuxFeedDoc(t *testing.T) {
	d := feedDoc{
		Version: "0.44.0",
		URL:     "https://downloads.cursor.com/grokbot/stable/12dcfa973ef51585fd1b35df6839fc9d1d7fd6aa/linux/x64/Grok_Bot_0.44.0.AppImage",
	}
	rel, ok := fromFeedDoc(d)
	if !ok {
		t.Fatal("expected ok")
	}
	if rel.Version != "0.44.0" || rel.BuildID != "12dcfa973ef51585fd1b35df6839fc9d1d7fd6aa" {
		t.Fatalf("bad release: %+v", rel)
	}
	want := "https://downloads.cursor.com/grokbot/stable/12dcfa973ef51585fd1b35df6839fc9d1d7fd6aa/linux/x64/grok-bot_0.44.0_amd64.deb"
	if rel.DebAMD64 != want {
		t.Fatalf("got %q want %q", rel.DebAMD64, want)
	}
}

func TestLatestAgainstStub(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"version":"9.9.9","url":"https://downloads.cursor.com/grokbot/stable/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/linux/x64/Grok_Bot_9.9.9.AppImage"}`))
	}))
	defer srv.Close()
	c := NewClient()
	old := LinuxX64Feed
	LinuxX64Feed = srv.URL
	defer func() { LinuxX64Feed = old }()
	rel, err := c.Latest()
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "9.9.9" || len(rel.BuildID) != 40 {
		t.Fatalf("bad: %+v", rel)
	}
}

func TestParseDownloadPage(t *testing.T) {
	html := `<a href="https://downloads.cursor.com/grokbot/stable/12dcfa973ef51585fd1b35df6839fc9d1d7fd6aa/linux/x64/grok-bot_0.44.0_amd64.deb">x64</a>` +
		`<a href="https://downloads.cursor.com/grokbot/stable/12dcfa973ef51585fd1b35df6839fc9d1d7fd6aa/linux/arm64/grok-bot_0.44.0_arm64.deb">arm64</a>`
	rel, err := ParseDownloadPage(html)
	if err != nil {
		t.Fatal(err)
	}
	if rel.Version != "0.44.0" {
		t.Fatalf("version %q", rel.Version)
	}
	if !strings.Contains(rel.DebAMD64, "amd64.deb") || !strings.Contains(rel.DebARM64, "arm64.deb") {
		t.Fatalf("bad urls: %+v", rel)
	}
}
