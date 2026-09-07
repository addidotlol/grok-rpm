// Package rpmbuild converts a Grok Bot .deb into an installable RPM.
//
// Pipeline:
//
//  1. Read DEBIAN/control from the .deb (deb.ReadControl).
//  2. Translate Depends/Recommends via depmap to RPM Requires.
//  3. Extract data.tar.* into rpmbuild BUILDROOT.
//  4. Apply RPM-isms: /usr/bin/grok-bot symlink (the .deb creates it in
//     postinst via update-alternatives), chrome-sandbox 4755, desktop
//     database scriptlets.
//  5. Render a spec file and run `rpmbuild -bb`.
//
// Debian-only maintainer-script work (apt source registration, AppArmor
// profile install) is intentionally not carried over; %post handles the
// desktop/sandbox bits that matter on Fedora/RHEL.
package rpmbuild

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"github.com/addidotlol/grok-rpm/internal/deb"
	"github.com/addidotlol/grok-rpm/internal/depmap"
)

// Options controls one conversion.
type Options struct {
	DebPath  string
	OutDir   string // where the finished .rpm is copied
	WorkDir  string // scratch root (rpmbuild _topdir lives under it)
	Release  string // RPM release, default "1"
	Packager string // optional RPM packager string
	// TargetArch overrides detection ("x86_64" or "aarch64").
	TargetArch string
}

// Result describes the built RPM.
type Result struct {
	RPMPath  string
	SpecPath string
	Name     string
	Version  string
	Release  string
	Arch     string
	Requires []string
	Unmapped []string
}

func debArchToRPM(arch string) string {
	switch strings.ToLower(arch) {
	case "arm64":
		return "aarch64"
	case "amd64", "x86_64":
		return "x86_64"
	default:
		return "x86_64"
	}
}

var specTmpl = template.Must(template.New("spec").Parse(`Name:           {{.Name}}
Version:        {{.Version}}
Release:        {{.Release}}%{?dist}
Summary:        Grok Bot desktop agent (converted from official .deb)
License:        Proprietary
URL:            https://cursor.com/download/bot
Source0:        {{.Source0}}
BuildArch:      {{.Arch}}

%global debug_package %{nil}
%global __strip /bin/true
%global _build_id_links none
%global __brp_check_rpaths %{nil}
AutoReq:        no
AutoProv:       no

{{range .Requires}}Requires:       {{.}}
{{end}}{{range .Recommends}}Recommends:      {{.}}
{{end}}Requires:       /bin/sh
Requires:       hicolor-icon-theme
{{range .Conflicts}}Conflicts:       {{.}}
{{end}}{{range .Provides}}Provides:        {{.}}
{{end}}
%description
{{.Description}}

This RPM is converted from the official Grok Bot .deb
({{.DebFileName}}; upstream {{.UpstreamPackage}} {{.UpstreamVersion}}).
Debian dependency names were translated to Fedora capabilities; see the
spec Requires list. Debian-only postinst work (apt source registration,
AppArmor profile) is intentionally omitted.

%install
rm -rf %{buildroot}
mkdir -p %{buildroot}
cp -a %{_sourcedir}/payload/. %{buildroot}/
# The .deb creates /usr/bin/grok-bot in postinst via update-alternatives;
# RPMs must ship the symlink explicitly.
mkdir -p %{buildroot}/usr/bin
ln -sf "/opt/Grok Bot/grok-bot" "%{buildroot}/usr/bin/grok-bot" || true
chmod 4755 "%{buildroot}/opt/Grok Bot/chrome-sandbox" || true

%post
if [ -x /opt/Grok\ Bot/chrome-sandbox ]; then chmod 4755 "/opt/Grok Bot/chrome-sandbox" || true; fi
if command -v update-desktop-database >/dev/null 2>&1; then update-desktop-database /usr/share/applications >/dev/null 2>&1 || true; fi
if command -v update-mime-database >/dev/null 2>&1; then update-mime-database /usr/share/mime >/dev/null 2>&1 || true; fi
if command -v gtk-update-icon-cache >/dev/null 2>&1; then gtk-update-icon-cache -f -t /usr/share/icons/hicolor >/dev/null 2>&1 || true; fi

%postun
if [ "$1" = "0" ]; then
  if command -v update-desktop-database >/dev/null 2>&1; then update-desktop-database /usr/share/applications >/dev/null 2>&1 || true; fi
  if command -v gtk-update-icon-cache >/dev/null 2>&1; then gtk-update-icon-cache -f -t /usr/share/icons/hicolor >/dev/null 2>&1 || true; fi
fi

%files
%defattr(-,root,root,-)
{{range .Files}}{{.}}
{{end}}
`))

type specData struct {
	Name            string
	Version         string
	Release         string
	Arch            string
	Source0         string
	Requires        []string
	Recommends      []string
	Conflicts       []string
	Provides        []string
	Description     string
	DebFileName     string
	UpstreamPackage string
	UpstreamVersion string
	Files           []string
}

// Convert runs the full .deb -> .rpm conversion.
func Convert(o Options) (Result, error) {
	var res Result
	if o.DebPath == "" {
		return res, fmt.Errorf("deb path required")
	}
	if o.Release == "" {
		o.Release = "1"
	}
	ctl, err := deb.ReadControl(o.DebPath)
	if err != nil {
		return res, fmt.Errorf("read control: %w", err)
	}
	if ctl.Package == "" {
		return res, fmt.Errorf("%s: control has no Package field", o.DebPath)
	}
	if ctl.Version == "" {
		return res, fmt.Errorf("%s: control has no Version field", o.DebPath)
	}
	rpmVersion := strings.ReplaceAll(ctl.Version, "-", "_")
	arch := o.TargetArch
	if arch == "" {
		arch = debArchToRPM(ctl.Architecture)
	}
	requires, unmapped := depmap.MapList(ctl.Depends)
	recommends, unmappedRec := depmap.MapList(ctl.Recommends)
	unmapped = append(unmapped, unmappedRec...)

	// Debian Conflicts/Provides usually name other packages (e.g. "sand");
	// carry the bare names over.
	var conflicts, provides []string
	for _, c := range ctl.Conflicts {
		if n := deb.DepName(c); n != "" {
			conflicts = append(conflicts, n)
		}
	}
	provides = append(provides, fmt.Sprintf("%s = %s-%s", ctl.Package, rpmVersion, o.Release))
	for _, p := range ctl.Provides {
		if n := deb.DepName(p); n != "" && n != ctl.Package {
			provides = append(provides, fmt.Sprintf("%s = %s", n, rpmVersion))
		}
	}

	work := o.WorkDir
	if work == "" {
		var err error
		work, err = os.MkdirTemp("", "grok-rpm-*")
		if err != nil {
			return res, err
		}
		defer os.RemoveAll(work)
	} else if err := os.MkdirAll(work, 0o755); err != nil {
		return res, err
	}
	topdir := filepath.Join(work, "rpmbuild")
	for _, d := range []string{"BUILD", "BUILDROOT", "RPMS", "SOURCES", "SPECS", "SRPMS"} {
		if err := os.MkdirAll(filepath.Join(topdir, d), 0o755); err != nil {
			return res, err
		}
	}
	// Extract payload into SOURCES/payload, then %install copies it.
	payloadDir := filepath.Join(topdir, "SOURCES", "payload")
	if err := os.MkdirAll(payloadDir, 0o755); err != nil {
		return res, err
	}
	if err := deb.ExtractData(o.DebPath, payloadDir); err != nil {
		return res, fmt.Errorf("extract data.tar: %w", err)
	}
	// Ensure the symlink target exists (payload has "opt/Grok Bot/grok-bot").
	// Nothing to do if upstream renames; %install ln -sf handles it.

	files, err := fileList(payloadDir)
	if err != nil {
		return res, err
	}

	desc := strings.TrimSpace(ctl.Description)
	if desc == "" {
		desc = "Grok Bot desktop agent."
	} else {
		// First line of Debian description is the short summary.
		if i := strings.Index(desc, "\n"); i >= 0 {
			desc = strings.TrimSpace(desc[:i]) + "\n" + strings.TrimSpace(desc[i+1:])
		}
	}

	data := specData{
		Name:            ctl.Package,
		Version:         rpmVersion,
		Release:         o.Release,
		Arch:            arch,
		Source0:         "payload",
		Requires:        requires,
		Recommends:      recommends,
		Conflicts:       conflicts,
		Provides:        provides,
		Description:     desc,
		DebFileName:     filepath.Base(o.DebPath),
		UpstreamPackage: ctl.Package,
		UpstreamVersion: ctl.Version,
		Files:           files,
	}
	specPath := filepath.Join(topdir, "SPECS", "grok-bot.spec")
	f, err := os.Create(specPath)
	if err != nil {
		return res, err
	}
	if err := specTmpl.Execute(f, data); err != nil {
		f.Close()
		return res, err
	}
	f.Close()

	if _, err := exec.LookPath("rpmbuild"); err != nil {
		return res, fmt.Errorf("rpmbuild not found in PATH (install rpm-build/rpm): %w", err)
	}
	args := []string{"-bb", "--define", "_topdir " + topdir}
	// Cross-arch payload: the files are prebuilt, so just label the arch.
	if arch == "aarch64" {
		args = append(args, "--target", "aarch64")
	}
	args = append(args, specPath)
	cmd := exec.Command("rpmbuild", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return res, fmt.Errorf("rpmbuild failed: %w", err)
	}
	built, err := filepath.Glob(filepath.Join(topdir, "RPMS", "*", "*.rpm"))
	if err != nil || len(built) == 0 {
		return res, fmt.Errorf("rpmbuild produced no RPM")
	}
	sort.Strings(built)
	src := built[len(built)-1]
	outDir := o.OutDir
	if outDir == "" {
		outDir = "."
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return res, err
	}
	dst := filepath.Join(outDir, filepath.Base(src))
	if err := copyFile(src, dst); err != nil {
		return res, err
	}
	res = Result{
		RPMPath: dst, SpecPath: specPath,
		Name: ctl.Package, Version: rpmVersion, Release: o.Release, Arch: arch,
		Requires: requires, Unmapped: unmapped,
	}
	return res, nil
}

// fileList walks root and returns %files entries. Directories are emitted
// as "%dir <path>"; the chrome-sandbox gets an explicit %attr(4755).
func fileList(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rpmPath := "/" + filepath.ToSlash(rel)
		// Quote paths with spaces (e.g. "/opt/Grok Bot/grok-bot").
		q := func(s string) string {
			if strings.ContainsAny(s, " \t\"") {
				return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
			}
			return s
		}
		switch {
		case info.IsDir():
			files = append(files, "%dir "+q(rpmPath))
		case info.Mode()&os.ModeSymlink != 0:
			files = append(files, q(rpmPath))
		default:
			if filepath.Base(rpmPath) == "chrome-sandbox" {
				files = append(files, fmt.Sprintf("%%attr(4755,root,root) %s", q(rpmPath)))
			} else {
				files = append(files, q(rpmPath))
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// The /usr/bin/grok-bot symlink is created in %install; list it too.
	files = append(files, "/usr/bin/grok-bot")
	sort.Strings(files)
	return files, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := out.ReadFrom(in); err != nil {
		return err
	}
	return out.Close()
}
