# Grok Bot RPM repo

Unofficial `dnf` repository with [Grok Bot](https://x.ai/bot) RPMs for Fedora/RHEL and derivatives, converted from the official Debian packages (including the dependency list). New upstream releases are picked up automatically, usually within a day.

> Unofficial. Not affiliated with xAI / Cursor. Grok Bot is proprietary; releases here are repackaged from the official `.deb` at build time.

## Install

x86_64 and aarch64:

```bash
sudo curl -L -o /etc/yum.repos.d/grok-bot.repo \
  https://raw.githubusercontent.com/addidotlol/grok-rpm/main/repo/grok-bot.repo
sudo dnf install -y grok-bot
```

Then launch `grok-bot` from your app menu or terminal. This installs the app under `/opt/Grok Bot`, a `/usr/bin/grok-bot` symlink, icons, and a desktop entry.

Updating works like any other package:

```bash
sudo dnf upgrade grok-bot
```

To remove:

```bash
sudo dnf remove -y grok-bot
sudo rm /etc/yum.repos.d/grok-bot.repo
```

Notes:

- The repo is unsigned (`gpgcheck=0`); packages come from this project's GitHub Actions builds, not from xAI.
- Prefer a manual download? Grab the `.rpm` for your arch from [Releases](https://github.com/addidotlol/grok-rpm/releases/latest) and `sudo dnf install ./grok-bot-*.rpm`.

## For maintainers

How releases are produced (fork this repo to run your own):

Upstream publishes Grok Bot under the update-service app name **`sand`**:

- `GET https://api2.cursor.sh/updates/api/update/linux-x64/sand/0.0.0/stable`
  → `{"version":"0.44.0","url":"https://downloads.cursor.com/grokbot/stable/<40-hex>/linux/x64/..."}`

The 40-hex path segment is the **build ID**. The `.deb` URLs are reconstructed from it:

- `.../stable/<build>/linux/x64/grok-bot_<ver>_amd64.deb`
- `.../stable/<build>/linux/arm64/grok-bot_<ver>_arm64.deb`

(`https://cursor.com/download/bot` is scraped as a fallback. The page also lists `Grok_Bot_<ver>.rpm` links, but those currently return HTTP 403 — which is why `.deb` → `.rpm` conversion is still needed.)

The Go app (`cmd/grok-rpm`):

1. **waits** — `daemon` polls the feeds on an interval; `sync --once` does one iteration (what CI runs).
2. **downloads** the official `.deb`(s).
3. **converts** each `.deb` to RPM:
   - parses `DEBIAN/control` with a built-in ar/xz/tar reader (no `dpkg` required),
   - translates every `Depends`/`Recommends` entry via `internal/depmap` (e.g. `libgtk-3-0` → `gtk3`, `libnss3` → `nss`, `libasound2t64 | libasound2` → `alsa-lib`),
   - stages the payload, ships the `/usr/bin/grok-bot` symlink the `.deb` only creates in `postinst`, sets `chrome-sandbox` to 4755, and builds with `rpmbuild -bb` (xz payload, since Ubuntu's gzip default pushes RPMs over GitHub's 100MB blob limit).
4. **makes a repository** — writes `repo/*.rpm` + `repo/repodata/repomd.xml` (+ `primary`/`filelists`/`other`). Uses `createrepo_c` when present, otherwise the built-in generator (verified with `dnf repoquery`).

Repo layout:

```
cmd/grok-rpm/          CLI (check/download/convert/repo/sync/daemon)
internal/feed/         upstream version discovery
internal/deb/          .deb control + payload reader
internal/depmap/       Debian -> Fedora dependency map
internal/rpmbuild/     spec rendering + rpmbuild invocation
internal/repo/         repodata generator + .repo file
internal/state/        VERSION/BUILD_ID persistence
repo/                  built RPMs (Git LFS) + repodata + grok-bot.repo
.github/workflows/sync.yml   daily + manual sync, release on new version
```

Run locally (Go 1.23+, `rpmbuild`):

```bash
go run ./cmd/grok-rpm check
go run ./cmd/grok-rpm sync --once --arch amd64 --state-dir . --repo-dir ./repo
```

`.github/workflows/sync.yml` runs `check` → per-arch `build` → `publish`: daily schedule plus manual dispatch (`version`, `arch`, `force`). Each arch builds on a native runner (`amd64` on `ubuntu-latest`, `arm64` on `ubuntu-24.04-arm`) because rpmbuild cannot cross-build. `publish` merges artifacts, regenerates repodata (`--keep 2`), commits `VERSION`/`BUILD_ID`/`repo/`, and cuts a `v<ver>` release. `repo/*.rpm` are Git LFS objects (contributors: `git lfs install && git lfs pull`); the `.repo` baseurl points at `raw.githubusercontent.com`, which resolves LFS — Pages would only serve pointers.
