// Package deb reads Debian package metadata and payloads without
// requiring dpkg on the host.
//
// A .deb is an ar archive with (at least) debian-binary, control.tar.* and
// data.tar.* members. Control holds the RFC822 "control" file (Package,
// Version, Depends, ...); data holds the filesystem payload.
package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/ulikunitz/xz"
)

// Control is the parsed subset of DEBIAN/control we care about.
type Control struct {
	Package      string
	Version      string
	Architecture string
	Maintainer   string
	Depends      []string // raw dependency tokens (alternatives kept as "a | b")
	Recommends   []string
	Suggests     []string
	Conflicts    []string
	Provides     []string
	Replaces     []string
	Description  string
	Raw          map[string]string
}

// Info describes a .deb file on disk.
type Info struct {
	Path     string
	Control  Control
	DataComp string // gz, xz, zst, or ""
}

// ParseControl parses an RFC822-style control file body.
func ParseControl(body string) Control {
	c := Control{Raw: map[string]string{}}
	var key, val string
	flush := func() {
		if key == "" {
			return
		}
		c.Raw[key] = val
		switch strings.ToLower(key) {
		case "package":
			c.Package = strings.TrimSpace(val)
		case "version":
			c.Version = strings.TrimSpace(val)
		case "architecture":
			c.Architecture = strings.TrimSpace(val)
		case "maintainer":
			c.Maintainer = strings.TrimSpace(val)
		case "depends":
			c.Depends = SplitDepList(val)
		case "recommends":
			c.Recommends = SplitDepList(val)
		case "suggests":
			c.Suggests = SplitDepList(val)
		case "conflicts":
			c.Conflicts = SplitDepList(val)
		case "provides":
			c.Provides = SplitDepList(val)
		case "replaces":
			c.Replaces = SplitDepList(val)
		case "description":
			c.Description = strings.TrimSpace(val)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			val += "\n" + strings.TrimPrefix(strings.TrimPrefix(line, " "), "\t")
			continue
		}
		flush()
		key, val = "", ""
		if i := strings.Index(line, ":"); i >= 0 {
			key = strings.TrimSpace(line[:i])
			val = strings.TrimSpace(line[i+1:])
		}
	}
	flush()
	return c
}

// SplitDepList splits a Debian dependency list on top-level commas,
// respecting parentheses (version constraints). Each element keeps any
// "|"-alternatives intact, e.g. "libasound2t64 | libasound2".
func SplitDepList(s string) []string {
	var out []string
	depth := 0
	start := 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				if t := strings.TrimSpace(s[start:i]); t != "" {
					out = append(out, t)
				}
				start = i + 1
			}
		}
	}
	if t := strings.TrimSpace(s[start:]); t != "" {
		out = append(out, t)
	}
	return out
}

// DepName strips version constraints, arch qualifiers and whitespace from
// one alternative, e.g. "libgtk-3-0 (>= 3.0) [amd64]" -> "libgtk-3-0".
func DepName(alt string) string {
	alt = strings.TrimSpace(alt)
	if i := strings.Index(alt, "("); i >= 0 {
		alt = strings.TrimSpace(alt[:i])
	}
	if i := strings.Index(alt, "["); i >= 0 {
		alt = strings.TrimSpace(alt[:i])
	}
	if i := strings.Index(alt, ":"); i >= 0 {
		// e.g. "libfoo:amd64" multi-arch qualifier
		alt = strings.TrimSpace(alt[:i])
	}
	return alt
}

// arMember is one member of the ar archive.
type arMember struct {
	name string
	data []byte
}

func readAr(path string) ([]arMember, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, 8)
	if _, err := io.ReadFull(f, magic); err != nil {
		return nil, fmt.Errorf("not an ar archive: %w", err)
	}
	if string(magic) != "!<arch>\n" {
		return nil, fmt.Errorf("not an ar archive: bad magic %q", magic)
	}
	var members []arMember
	for {
		hdr := make([]byte, 60)
		_, err := io.ReadFull(f, hdr)
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(strings.TrimSpace(string(hdr[0:16])), "/")
		name = strings.TrimSpace(name)
		size, err := strconv.Atoi(strings.TrimSpace(string(hdr[48:58])))
		if err != nil {
			return nil, fmt.Errorf("bad ar size: %w", err)
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(f, data); err != nil {
			return nil, err
		}
		if size%2 == 1 {
			f.Read(make([]byte, 1)) // padding
		}
		members = append(members, arMember{name: name, data: data})
	}
	return members, nil
}

func decompress(name string, data []byte) (io.Reader, error) {
	switch {
	case strings.HasSuffix(name, ".gz"):
		return gzip.NewReader(bytes.NewReader(data))
	case strings.HasSuffix(name, ".xz"):
		return xz.NewReader(bytes.NewReader(data))
	case strings.HasSuffix(name, ".zst") || strings.HasSuffix(name, ".zstd"):
		dec, err := zstd.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		return dec.IOReadCloser(), nil
	default: // uncompressed tar
		return bytes.NewReader(data), nil
	}
}

// ReadControl extracts and parses the control file from a .deb.
func ReadControl(debPath string) (Control, error) {
	members, err := readAr(debPath)
	if err != nil {
		return Control{}, err
	}
	for _, m := range members {
		if !strings.HasPrefix(m.name, "control.tar") {
			continue
		}
		r, err := decompress(m.name, m.data)
		if err != nil {
			return Control{}, err
		}
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return Control{}, err
			}
			base := filepath.Base(h.Name)
			if base == "control" {
				body, err := io.ReadAll(io.LimitReader(tr, 1<<20))
				if err != nil {
					return Control{}, err
				}
				if closer, ok := r.(io.Closer); ok {
					closer.Close()
				}
				return ParseControl(string(body)), nil
			}
		}
		if closer, ok := r.(io.Closer); ok {
			closer.Close()
		}
	}
	return Control{}, fmt.Errorf("%s: control.tar.* member not found", debPath)
}

// ExtractData extracts the data.tar.* payload into dest (which must exist).
// Permissions, symlinks and hardlinks are preserved; "./" prefixes are
// stripped; absolute paths and ".." are rejected.
func ExtractData(debPath, dest string) error {
	members, err := readAr(debPath)
	if err != nil {
		return err
	}
	var data *arMember
	for i, m := range members {
		if strings.HasPrefix(m.name, "data.tar") {
			data = &members[i]
			break
		}
	}
	if data == nil {
		return fmt.Errorf("%s: data.tar.* member not found", debPath)
	}
	r, err := decompress(data.name, data.data)
	if err != nil {
		return err
	}
	if closer, ok := r.(io.Closer); ok {
		defer closer.Close()
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimPrefix(name, "/")
		if name == "" || name == "." {
			continue
		}
		if strings.Contains(name, "..") {
			return fmt.Errorf("unsafe path in data.tar: %q", h.Name)
		}
		target := filepath.Join(dest, filepath.FromSlash(name))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(h.Mode)); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		case tar.TypeLink:
			src := filepath.Join(dest, filepath.FromSlash(strings.TrimPrefix(strings.TrimPrefix(h.Linkname, "./"), "/")))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			os.Remove(target)
			if err := os.Link(src, target); err != nil {
				return err
			}
		default:
			// Skip exotic entries (char/block/fifo).
		}
	}
}
