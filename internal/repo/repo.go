// Package repo generates a dnf-compatible RPM repository (repodata).
//
// If `createrepo_c` is on PATH it is used for full fidelity; otherwise a
// built-in minimal generator emits primary/filelists/other + repomd so the
// Go binary has no hard dependency on createrepo. RPM metadata is read via
// the `rpm` CLI (rpm -qp), which exists anywhere rpmbuild does.
package repo

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options for Generate.
type Options struct {
	RPMDir        string // directory containing *.rpm
	OutDir        string // repo root (defaults to RPMDir)
	RepoName      string // human name in .repo file, default "grok-bot"
	BaseURL       string // baseurl written into the .repo file
	UseCreaterepo bool   // use createrepo_c when available (default true)
}

// Generate builds/refreshes repodata for all *.rpm in RPMDir.
func Generate(o Options) error {
	if o.RPMDir == "" {
		return fmt.Errorf("rpm dir required")
	}
	out := o.OutDir
	if out == "" {
		out = o.RPMDir
	}
	rpms, err := filepath.Glob(filepath.Join(o.RPMDir, "*.rpm"))
	if err != nil {
		return err
	}
	if len(rpms) == 0 {
		return fmt.Errorf("no *.rpm found in %s", o.RPMDir)
	}
	if o.UseCreaterepo {
		if _, err := exec.LookPath("createrepo_c"); err == nil {
			cmd := exec.Command("createrepo_c", "--update", out)
			cmd.Stdout = os.Stderr
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err == nil {
				return writeRepoFile(out, o)
			}
			// fall through to built-in on failure
		}
	}
	return generateBuiltIn(out, rpms, o)
}

func writeRepoFile(out string, o Options) error {
	name := o.RepoName
	if name == "" {
		name = "grok-bot"
	}
	base := o.BaseURL
	if base == "" {
		base = "https://example.com/repo/"
	}
	content := fmt.Sprintf(`[%s]
name=%s Grok Bot RPM repository
baseurl=%s
enabled=1
gpgcheck=0
skip_if_unavailable=True
`, name, name, base)
	return os.WriteFile(filepath.Join(out, name+".repo"), []byte(content), 0o644)
}

// --- built-in repodata generator (minimal but dnf-compatible) ---

type pkgMeta struct {
	filename   string
	name       string
	arch       string
	version    string
	release    string
	epoch      string
	summary    string
	desc       string
	packager   string
	url        string
	license    string
	buildTime  string
	sizePkg    int64
	sizeInst   int64
	sha256     string
	files      []string
	requires   []string
	provides   []string
	conflicts  []string
	obsoletes  []string
	recommends []string
}

func rpmQuery(path, format string) (string, error) {
	out, err := exec.Command("rpm", "-qp", "--queryformat", format, path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("rpm -qp %s: %v: %s", path, err, out)
	}
	return string(out), nil
}

func rpmLines(path, flag string) []string {
	out, err := exec.Command("rpm", "-qp", flag, path).CombinedOutput()
	if err != nil {
		return nil
	}
	var lines []string
	for _, l := range strings.Split(string(out), "\n") {
		if t := strings.TrimSpace(l); t != "" && t != "(none)" {
			lines = append(lines, t)
		}
	}
	return lines
}

func collect(path string) (pkgMeta, error) {
	var m pkgMeta
	m.filename = filepath.Base(path)
	// Use a unit-separator delimited single query to avoid many forks.
	const sep = "\x1f"
	q, err := rpmQuery(path, "%{NAME}"+sep+"%{ARCH}"+sep+"%{VERSION}"+sep+"%{RELEASE}"+sep+"%{EPOCH}"+sep+"%{SUMMARY}"+sep+"%{DESCRIPTION}"+sep+"%{PACKAGER}"+sep+"%{URL}"+sep+"%{LICENSE}"+sep+"%{BUILDTIME}"+sep+"%{SIZE}")
	if err != nil {
		return m, err
	}
	parts := strings.Split(q, sep)
	if len(parts) < 11 {
		return m, fmt.Errorf("unexpected rpm query output for %s", path)
	}
	m.name, m.arch, m.version, m.release, m.epoch = parts[0], parts[1], parts[2], parts[3], parts[4]
	if m.epoch == "(none)" {
		m.epoch = "0"
	}
	m.summary, m.desc, m.packager, m.url, m.license, m.buildTime = parts[5], parts[6], parts[7], parts[8], parts[9], parts[10]
	st, err := os.Stat(path)
	if err != nil {
		return m, err
	}
	m.sizePkg = st.Size()
	var inst int64
	if s, err := rpmQuery(path, "%{SIZE}"); err == nil {
		fmt.Sscan(strings.TrimSpace(s), &inst)
	}
	m.sizeInst = inst
	h := sha256.New()
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err != nil {
			break
		}
	}
	m.sha256 = hex.EncodeToString(h.Sum(nil))
	// File list.
	for _, l := range rpmLines(path, "--list") {
		m.files = append(m.files, strings.TrimSpace(l))
	}
	m.requires = rpmLines(path, "--requires")
	m.provides = rpmLines(path, "--provides")
	m.conflicts = rpmLines(path, "--conflicts")
	m.obsoletes = rpmLines(path, "--obsoletes")
	m.recommends = rpmLines(path, "--recommends")
	return m, nil
}

func generateBuiltIn(out string, rpms []string, o Options) error {
	sort.Strings(rpms)
	var pkgs []pkgMeta
	for _, r := range rpms {
		m, err := collect(r)
		if err != nil {
			return err
		}
		pkgs = append(pkgs, m)
	}
	repomd := repomdXML{Xmlns: "http://linux.duke.edu/metadata/repo", Revision: time.Now().UTC().Unix()}
	writeGz := func(name string, data []byte) (string, error) {
		sum := sha256.Sum256(data)
		hexsum := hex.EncodeToString(sum[:])
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		gz.Header.Name = name
		gz.Header.ModTime = time.Now()
		if _, err := gz.Write(data); err != nil {
			return "", err
		}
		gz.Close()
		// createrepo_c convention: <sha256>-<name>.gz so dnf auto-decompresses.
		final := hexsum + "-" + name + ".gz"
		if err := os.MkdirAll(filepath.Join(out, "repodata"), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(out, "repodata", final), buf.Bytes(), 0o644); err != nil {
			return "", err
		}
		return "repodata/" + final, nil
	}

	primary := buildPrimary(pkgs)
	ploc, err := writeGz("primary.xml", primary)
	if err != nil {
		return err
	}
	fl := buildFilelists(pkgs)
	floc, err := writeGz("filelists.xml", fl)
	if err != nil {
		return err
	}
	other := buildOther(pkgs)
	oloc, err := writeGz("other.xml", other)
	if err != nil {
		return err
	}
	for _, e := range []struct {
		typ, href string
		data      []byte
	}{
		{"primary", ploc, primary},
		{"filelists", floc, fl},
		{"other", oloc, other},
	} {
		var raw []byte
		p := filepath.Join(out, e.href)
		if gz, err := os.ReadFile(p); err == nil {
			// checksum of the compressed file for repomd
			_ = gz
		}
		raw, _ = os.ReadFile(p)
		sum := sha256.Sum256(raw)
		repomd.Data = append(repomd.Data, repomdData{
			Type:     e.typ,
			Checksum: repomdChecksum{Type: "sha256", Value: hex.EncodeToString(sum[:])},
			Location: repomdLocation{Href: e.href},
			Size:     int64(len(raw)),
		})
		_ = dataLen(e.data)
	}
	doc, _ := xml.MarshalIndent(repomd, "", "  ")
	doc = append([]byte(xml.Header), doc...)
	if err := os.WriteFile(filepath.Join(out, "repodata", "repomd.xml"), doc, 0o644); err != nil {
		return err
	}
	return writeRepoFile(out, o)
}

func dataLen(b []byte) int { return len(b) }

// --- minimal repodata XML shapes ---

type repomdXML struct {
	XMLName  xml.Name     `xml:"repomd"`
	Xmlns    string       `xml:"xmlns,attr"`
	Revision int64        `xml:"revision"`
	Data     []repomdData `xml:"data"`
}
type repomdData struct {
	Type     string         `xml:"type,attr"`
	Checksum repomdChecksum `xml:"checksum"`
	Location repomdLocation `xml:"location"`
	Size     int64          `xml:"size"`
}
type repomdChecksum struct {
	Type  string `xml:"type,attr"`
	Value string `xml:",chardata"`
}
type repomdLocation struct {
	Href string `xml:"href,attr"`
}

func xmlEscape(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func rpmEntry(tag, name string) string {
	// "name" may be "capability" or "name = version" style from rpm output;
	// keep it verbatim inside <rpm:entry name="..."/>.
	parts := strings.Fields(name)
	entryName := name
	if len(parts) > 0 {
		entryName = parts[0]
	}
	return fmt.Sprintf(`<%s name=%q/>`, tag, entryName)
}

func buildPrimary(pkgs []pkgMeta) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<metadata xmlns="http://linux.duke.edu/metadata/common" xmlns:rpm="http://linux.duke.edu/metadata/rpm" packages="` + fmt.Sprint(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package type=\"rpm\">\n<name>%s</name>\n<arch>%s</arch>\n", xmlEscape(p.name), xmlEscape(p.arch))
		fmt.Fprintf(&b, "<version epoch=%q ver=%q rel=%q/>\n", xmlEscape(p.epoch), xmlEscape(p.version), xmlEscape(p.release))
		fmt.Fprintf(&b, "<checksum type=\"sha256\" pkgid=\"YES\">%s</checksum>\n", p.sha256)
		fmt.Fprintf(&b, "<summary>%s</summary>\n<description>%s</description>\n", xmlEscape(p.summary), xmlEscape(p.desc))
		fmt.Fprintf(&b, "<packager>%s</packager>\n<url>%s</url>\n", xmlEscape(p.packager), xmlEscape(p.url))
		fmt.Fprintf(&b, "<time file=%q build=%q/>\n", xmlEscape(p.buildTime), xmlEscape(p.buildTime))
		fmt.Fprintf(&b, "<size package=%q installed=%q archive=%q/>\n", fmt.Sprint(p.sizePkg), fmt.Sprint(p.sizeInst), fmt.Sprint(p.sizeInst))
		fmt.Fprintf(&b, "<location href=%q/>\n", p.filename)
		b.WriteString("<format>\n")
		fmt.Fprintf(&b, "<rpm:license>%s</rpm:license>\n", xmlEscape(p.license))
		fmt.Fprintf(&b, "<rpm:vendor>%s</rpm:vendor>\n", xmlEscape("Grok Bot (converted)"))
		b.WriteString("<rpm:provides>\n")
		for _, s := range p.provides {
			b.WriteString(rpmEntry("rpm:entry", s) + "\n")
		}
		b.WriteString("</rpm:provides>\n<rpm:requires>\n")
		for _, s := range p.requires {
			if strings.HasPrefix(s, "rpmlib(") {
				continue
			}
			b.WriteString(rpmEntry("rpm:entry", s) + "\n")
		}
		b.WriteString("</rpm:requires>\n<rpm:conflicts>\n")
		for _, s := range p.conflicts {
			b.WriteString(rpmEntry("rpm:entry", s) + "\n")
		}
		b.WriteString("</rpm:conflicts>\n<rpm:obsoletes>\n")
		for _, s := range p.obsoletes {
			b.WriteString(rpmEntry("rpm:entry", s) + "\n")
		}
		b.WriteString("</rpm:obsoletes>\n<rpm:recommends>\n")
		for _, s := range p.recommends {
			b.WriteString(rpmEntry("rpm:entry", s) + "\n")
		}
		b.WriteString("</rpm:recommends>\n</format>\n</package>\n")
	}
	b.WriteString("</metadata>\n")
	return []byte(b.String())
}

func buildFilelists(pkgs []pkgMeta) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<filelists xmlns="http://linux.duke.edu/metadata/filelists" packages="` + fmt.Sprint(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=%q name=%q arch=%q>\n<version epoch=%q ver=%q rel=%q/>\n", p.sha256, xmlEscape(p.name), xmlEscape(p.arch), xmlEscape(p.epoch), xmlEscape(p.version), xmlEscape(p.release))
		for _, f := range p.files {
			fmt.Fprintf(&b, "<file>%s</file>\n", xmlEscape(f))
		}
		b.WriteString("</package>\n")
	}
	b.WriteString("</filelists>\n")
	return []byte(b.String())
}

func buildOther(pkgs []pkgMeta) []byte {
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString(`<otherdata xmlns="http://linux.duke.edu/metadata/other" packages="` + fmt.Sprint(len(pkgs)) + `">` + "\n")
	for _, p := range pkgs {
		fmt.Fprintf(&b, "<package pkgid=%q name=%q arch=%q>\n<version epoch=%q ver=%q rel=%q/>\n<changelog author=%q date=%q>Converted from official .deb</changelog>\n</package>\n",
			p.sha256, xmlEscape(p.name), xmlEscape(p.arch), xmlEscape(p.epoch), xmlEscape(p.version), xmlEscape(p.release), "grok-rpm", p.buildTime)
	}
	b.WriteString("</otherdata>\n")
	return []byte(b.String())
}
