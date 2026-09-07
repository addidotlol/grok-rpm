// Command grok-rpm watches Grok Bot upstream, converts the official .deb
// to RPM (translating the Debian dependency list), and maintains a
// dnf-compatible RPM repository.
//
//	# One-shot (for GitHub Actions):
//	grok-rpm sync --once --arch all --state-dir . --repo-dir ./repo
//
//	# Long-running watcher ("waits"):
//	grok-rpm daemon --poll-interval 6h
//
//	# Inspect only:
//	grok-rpm check --format json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/addidotlol/grok-rpm/internal/deb"
	"github.com/addidotlol/grok-rpm/internal/feed"
	"github.com/addidotlol/grok-rpm/internal/repo"
	"github.com/addidotlol/grok-rpm/internal/rpmbuild"
	"github.com/addidotlol/grok-rpm/internal/state"
)

const usage = `grok-rpm - Grok Bot .deb -> RPM converter + repo builder

Usage:
  grok-rpm <command> [flags]

Commands:
  check      print latest upstream release (no download)
  download   download official .deb(s)
  convert    convert one .deb to .rpm
  repo       (re)generate repodata for a directory of .rpms
  sync       check + download + convert + repo (+ state update)
  daemon     run sync in a poll loop forever ("waits" for updates)

Run "grok-rpm <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "check":
		err = runCheck(args)
	case "download":
		err = runDownload(args)
	case "convert":
		err = runConvert(args)
	case "repo":
		err = runRepo(args)
	case "sync":
		err = runSync(args)
	case "daemon":
		err = runDaemon(args)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(os.Stderr, usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// ---------- check ----------

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	format := fs.String("format", "text", "output format: text|json")
	fs.Parse(args)
	rel, err := feed.NewClient().Latest()
	if err != nil {
		return err
	}
	if *format == "json" {
		return json.NewEncoder(os.Stdout).Encode(rel)
	}
	fmt.Printf("version=%s\nbuild_id=%s\ndeb_amd64=%s\ndeb_arm64=%s\n",
		rel.Version, rel.BuildID, rel.DebAMD64, rel.DebARM64)
	return nil
}

// ---------- download ----------

func runDownload(args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	arch := fs.String("arch", "amd64", "amd64|arm64|all")
	outdir := fs.String("outdir", ".work/debs", "download directory")
	versionPin := fs.String("version", "", "pin version (must match feed or state)")
	stateDir := fs.String("state-dir", ".", "directory with VERSION/BUILD_ID")
	fs.Parse(args)
	rel, err := feed.NewClient().Latest()
	if err != nil {
		return err
	}
	if *versionPin != "" && *versionPin != rel.Version {
		// Allow rebuild of the recorded version with its recorded hash.
		st, _ := state.Load(*stateDir)
		if *versionPin == st.Version && st.BuildID != "" {
			rel.Version = st.Version
			rel.BuildID = st.BuildID
			rel.DownloadBase = "https://downloads.cursor.com/grokbot/stable"
			rel.DebAMD64 = fmt.Sprintf("%s/%s/linux/x64/grok-bot_%s_amd64.deb", rel.DownloadBase, st.BuildID, st.Version)
			rel.DebARM64 = fmt.Sprintf("%s/%s/linux/arm64/grok-bot_%s_arm64.deb", rel.DownloadBase, st.BuildID, st.Version)
		} else {
			return fmt.Errorf("pinned version %s != feed version %s (cannot resolve build hash for old versions)", *versionPin, rel.Version)
		}
	}
	archs := expandArch(*arch)
	if err := os.MkdirAll(*outdir, 0o755); err != nil {
		return err
	}
	for _, a := range archs {
		url := rel.DebURL(a)
		dst := filepath.Join(*outdir, filepath.Base(url))
		fmt.Fprintf(os.Stderr, "downloading %s -> %s\n", url, dst)
		if err := downloadFile(url, dst); err != nil {
			return err
		}
		fmt.Println(dst)
	}
	return nil
}

func expandArch(a string) []string {
	switch strings.ToLower(a) {
	case "all":
		return []string{"amd64", "arm64"}
	case "arm64", "aarch64":
		return []string{"arm64"}
	default:
		return []string{"amd64"}
	}
}

func downloadFile(url, dst string) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "grok-rpm/1.0")
	client := &http.Client{Timeout: 0}
	// Simple retry loop.
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 5 * time.Second)
		}
		if err := func() error {
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
			}
			tmp := dst + ".part"
			f, err := os.Create(tmp)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, resp.Body)
			f.Close()
			if err != nil {
				os.Remove(tmp)
				return err
			}
			if n == 0 {
				os.Remove(tmp)
				return fmt.Errorf("empty download: %s", url)
			}
			return os.Rename(tmp, dst)
		}(); err != nil {
			last = err
			fmt.Fprintf(os.Stderr, "attempt %d failed: %v\n", attempt+1, err)
			continue
		}
		return nil
	}
	return last
}

// ---------- convert ----------

func runConvert(args []string) error {
	fs := flag.NewFlagSet("convert", flag.ExitOnError)
	debPath := fs.String("deb", "", "input .deb path")
	outdir := fs.String("outdir", ".work/rpms", "output directory for .rpm")
	workdir := fs.String("workdir", ".work/convert", "scratch directory")
	release := fs.String("release", "1", "RPM release")
	fs.Parse(args)
	if *debPath == "" && fs.NArg() > 0 {
		*debPath = fs.Arg(0)
	}
	if *debPath == "" {
		return fmt.Errorf("--deb required")
	}
	ctl, err := deb.ReadControl(*debPath)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "converting %s (Package=%s Version=%s Arch=%s)\n",
		*debPath, ctl.Package, ctl.Version, ctl.Architecture)
	fmt.Fprintf(os.Stderr, "debian depends: %s\n", strings.Join(ctl.Depends, ", "))
	res, err := rpmbuild.Convert(rpmbuild.Options{
		DebPath: *debPath, OutDir: *outdir, WorkDir: *workdir, Release: *release,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "rpm requires: %s\n", strings.Join(res.Requires, ", "))
	if len(res.Unmapped) > 0 {
		fmt.Fprintf(os.Stderr, "warning: unmapped debian deps (kept verbatim): %s\n", strings.Join(res.Unmapped, ", "))
	}
	fmt.Println(res.RPMPath)
	return nil
}

// ---------- repo ----------

func runRepo(args []string) error {
	fs := flag.NewFlagSet("repo", flag.ExitOnError)
	rpmdir := fs.String("rpmdir", "./repo", "directory with *.rpm")
	outdir := fs.String("outdir", "", "repo root (default = rpmdir)")
	name := fs.String("name", "grok-bot", "repo id/name")
	baseurl := fs.String("baseurl", "", "baseurl for .repo file")
	fs.Parse(args)
	out := *outdir
	if out == "" {
		out = *rpmdir
	}
	return repo.Generate(repo.Options{RPMDir: *rpmdir, OutDir: out, RepoName: *name, BaseURL: *baseurl})
}

// ---------- sync ----------

func runSync(args []string) error {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	arch := fs.String("arch", "all", "amd64|arm64|all")
	stateDir := fs.String("state-dir", ".", "directory with VERSION/BUILD_ID")
	repoDir := fs.String("repo-dir", "./repo", "RPM repository directory")
	workdir := fs.String("workdir", ".work", "scratch directory")
	release := fs.String("release", "1", "RPM release")
	once := fs.Bool("once", false, "single iteration (for CI)")
	force := fs.Bool("force", false, "rebuild even if version unchanged")
	versionPin := fs.String("version", "", "pin version (rebuild only)")
	keep := fs.Int("keep", 2, "keep N newest RPMs per arch (0 = keep all)")
	baseurl := fs.String("baseurl", "", "baseurl for .repo file")
	_ = once // sync is always single-iteration; daemon loops
	fs.Parse(args)

	rel, err := feed.NewClient().Latest()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "upstream: version=%s build=%s\n", rel.Version, rel.BuildID)
	if *versionPin != "" && *versionPin != rel.Version {
		st, _ := state.Load(*stateDir)
		if *versionPin == st.Version && st.BuildID != "" {
			fmt.Fprintf(os.Stderr, "pin %s: rebuilding recorded build %s\n", *versionPin, st.BuildID)
			rel.Version, rel.BuildID = st.Version, st.BuildID
			rel.DebAMD64 = fmt.Sprintf("%s/%s/linux/x64/grok-bot_%s_amd64.deb", rel.DownloadBase, st.BuildID, st.Version)
			rel.DebARM64 = fmt.Sprintf("%s/%s/linux/arm64/grok-bot_%s_arm64.deb", rel.DownloadBase, st.BuildID, st.Version)
		} else {
			return fmt.Errorf("pinned %s != feed %s and no recorded build hash", *versionPin, rel.Version)
		}
	}
	st, err := state.Load(*stateDir)
	if err != nil {
		return err
	}
	if !*force && !st.IsNew(rel.Version, rel.BuildID) {
		fmt.Fprintf(os.Stderr, "up to date (%s); nothing to do (use --force to rebuild)\n", st.Version)
		return nil
	}

	debDir := filepath.Join(*workdir, "debs")
	rpmOut := filepath.Join(*workdir, "rpms")
	os.MkdirAll(debDir, 0o755)
	os.MkdirAll(rpmOut, 0o755)
	os.MkdirAll(*repoDir, 0o755)

	built := 0
	for _, a := range expandArch(*arch) {
		url := rel.DebURL(a)
		debPath := filepath.Join(debDir, filepath.Base(url))
		fmt.Fprintf(os.Stderr, "downloading %s\n", url)
		if err := downloadFile(url, debPath); err != nil {
			// arm64 may 404 on some releases; warn and continue if amd64 ok.
			fmt.Fprintf(os.Stderr, "warning: %s: %v\n", url, err)
			continue
		}
		res, err := rpmbuild.Convert(rpmbuild.Options{
			DebPath: debPath, OutDir: *repoDir,
			WorkDir: filepath.Join(*workdir, "convert-"+a), Release: *release,
		})
		if err != nil {
			return fmt.Errorf("convert %s: %w", a, err)
		}
		fmt.Fprintf(os.Stderr, "built %s\n", res.RPMPath)
		if len(res.Unmapped) > 0 {
			fmt.Fprintf(os.Stderr, "warning: unmapped deps: %s\n", strings.Join(res.Unmapped, ", "))
		}
		built++
		_ = rpmOut
	}
	if built == 0 {
		return fmt.Errorf("no packages built")
	}
	if *keep > 0 {
		pruneOld(*repoDir, *keep)
	}
	if err := repo.Generate(repo.Options{RPMDir: *repoDir, OutDir: *repoDir, RepoName: "grok-bot", BaseURL: *baseurl}); err != nil {
		return fmt.Errorf("repo: %w", err)
	}
	if err := state.Save(*stateDir, state.State{Version: rel.Version, BuildID: rel.BuildID}); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "synced %s (%s)\n", rel.Version, rel.BuildID)
	return nil
}

func pruneOld(repoDir string, keep int) {
	for _, pat := range []string{"*x86_64.rpm", "*aarch64.rpm", "*arm64.rpm"} {
		matches, _ := filepath.Glob(filepath.Join(repoDir, pat))
		if len(matches) <= keep {
			continue
		}
		type fi struct {
			p string
			t time.Time
		}
		var list []fi
		for _, m := range matches {
			st, err := os.Stat(m)
			if err != nil {
				continue
			}
			list = append(list, fi{m, st.ModTime()})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
		for _, old := range list[keep:] {
			fmt.Fprintf(os.Stderr, "pruning %s (keep=%d)\n", old.p, keep)
			os.Remove(old.p)
		}
	}
}

// ---------- daemon ----------

func runDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	interval := fs.Duration("poll-interval", 6*time.Hour, "poll interval (e.g. 30m, 6h)")
	// Pass-through sync flags.
	arch := fs.String("arch", "all", "amd64|arm64|all")
	stateDir := fs.String("state-dir", ".", "directory with VERSION/BUILD_ID")
	repoDir := fs.String("repo-dir", "./repo", "RPM repository directory")
	workdir := fs.String("workdir", ".work", "scratch directory")
	release := fs.String("release", "1", "RPM release")
	keep := fs.Int("keep", 2, "keep N newest RPMs per arch")
	baseurl := fs.String("baseurl", "", "baseurl for .repo file")
	fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "grok-rpm daemon: polling every %s\n", interval)
	// Run once immediately, then wait.
	doSync := func() {
		err := runSync([]string{
			"--arch", *arch, "--state-dir", *stateDir, "--repo-dir", *repoDir,
			"--workdir", *workdir, "--release", *release,
			"--keep", fmt.Sprint(*keep), "--baseurl", *baseurl,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "sync failed: %v\n", err)
		}
	}
	doSync()
	t := time.NewTicker(*interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "shutting down")
			return nil
		case <-t.C:
			doSync()
		}
	}
}
