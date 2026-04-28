# Linux Distribution Packaging — Design Spec

**Date:** 2026-04-27
**Project:** agwkiss
**Status:** Approved

---

## Goal

Produce native Linux packages (`.deb`, Fedora `.rpm`, openSUSE `.rpm`, and Arch
`PKGBUILD`) that are built automatically on each tagged GitHub release and published
via openSUSE Build Service (OBS) repos so users can install with their distro's
native package manager.

---

## Target Distributions

| Format | Distros covered |
|--------|----------------|
| `.deb` | Debian, Ubuntu, Raspberry Pi OS, Linux Mint |
| `.rpm` (Fedora) | Fedora, RHEL, CentOS Stream |
| `.rpm` (openSUSE) | openSUSE Leap, Tumbleweed |
| `PKGBUILD` | Arch Linux, Manjaro (reference / AUR) |

---

## Distribution Method

- **OBS** builds and hosts `.deb` and `.rpm` packages in per-distro repos. Users
  add the repo once and install/update with `apt`/`dnf`/`zypper`.
- **PKGBUILD** is maintained in the GitHub repo under `packaging/PKGBUILD` as a
  reference. It can be submitted to AUR for `yay -S agwkiss` support.

---

## Dependency Strategy

agwkiss requires three Python packages: `kiss3 >= 8.0.0`, `pyham-ax25 >= 1.0.0`,
and `pyserial >= 3.5`.

| Package | Status | Action |
|---------|--------|--------|
| `pyserial` | In all major distro repos | No packaging needed |
| `pyham-ax25 >= 1.0.0` | Already in OBS | Reference via OBS project dependency |
| `kiss3 >= 8.0.0` | Not in OBS or distro repos | Create `python-kiss3` package in our OBS project |

The OBS project config declares a dependency on the OBS project hosting
`python-pyham-ax25`. Updates to that package flow in automatically with no
action required on our side.

---

## OBS Project Structure

Project: `home:CALLSIGN:agwkiss` (or a shared hamradio project if one exists)

```
home:CALLSIGN:agwkiss/
├── _config               # project-level repo/dependency declarations (set up once in UI)
├── python-kiss3/
│   ├── _service          # pulls sdist from PyPI via obs_pypi service
│   └── python-kiss3.spec # RPM + Debian metadata
└── agwkiss/
    ├── _service          # pulls tarball from GitHub tag via obs_scm
    └── agwkiss.spec      # RPM spec (Debian packaging lives in debian/ inside source tree)
```

OBS builds all targets in parallel from these files. No build servers to manage.

### Debian packaging in OBS

For Debian builds, OBS uses the `debian/` directory bundled inside the source
tarball fetched by `obs_scm`. The `debian/` directory must be present in the
GitHub source tree (at `packaging/debian/` — see below). OBS generates the
`.dsc` + `.orig.tar.gz` + `.debian.tar.gz` internally from these inputs. No
separate `.dsc` file is committed to OBS.

The `set_version` service rewrites the version string in RPM spec files
automatically. For `debian/changelog` it does **not** do so unconditionally —
the top entry in `debian/changelog` must use a placeholder version (e.g.,
`0.0.0`) that `set_version` can substitute. The `changesgenerate` param on
`obs_scm` produces an OBS-side `.changes` file for RPM but does not update
`debian/changelog`. In practice this means `debian/changelog` is maintained
with a single `UNRELEASED` placeholder entry at `0.0.0`; OBS's `set_version`
service replaces it on each build. Verify this works during initial OBS setup —
if `set_version` does not handle the substitution, the fallback is to update
`debian/changelog` in the CI step that pushes to OBS (same sed pass that updates
the spec `Version:`).

---

## GitHub Repository Layout

```
packaging/
├── PKGBUILD                  # Arch reference / AUR submission
├── agwkiss.spec              # RPM spec (canonical copy; CI syncs to OBS)
├── debian/                   # Debian packaging (bundled into source tarball by OBS)
│   ├── control               # package metadata + dependencies
│   ├── rules                 # build instructions (dh_python3)
│   ├── changelog             # required by dpkg; version updated by set_version service
│   └── copyright
└── obs/
    ├── agwkiss/_service      # OBS source service config for agwkiss
    ├── python-kiss3/_service # OBS source service config for kiss3
    └── python-kiss3/python-kiss3.spec
```

The `packaging/` files are the canonical source of truth. CI pushes the spec and
`_service` files to OBS on each release after substituting the version.

---

## Release Flow

```
git tag v1.x.x && git push --tags
        |
        v
GitHub Actions: release.yml
  1. Extract version from tag (v1.2.3 → 1.2.3)
  2. In memory: update Version: in agwkiss.spec
  3. In memory: update pkgver= in PKGBUILD and revision in agwkiss _service
  4. Commit updated spec + PKGBUILD to repo (see note on branch protection below)
  5. Configure osc and push updated files to OBS
        |
        v
OBS builds in parallel:
  - debian:12       → .deb
  - fedora:40       → .rpm
  - opensuse/leap   → .rpm
        |
        v
Packages available in OBS repo for all targets
```

### Branch protection note

The version-bump commit (step 4) writes back to the repo so the checked-in spec
and PKGBUILD stay in sync with releases. If the repo has branch protection requiring
PR reviews, this push will fail with a default `GITHUB_TOKEN`. Two options:

- **Preferred**: Exempt the `release.yml` workflow from branch protection using a
  dedicated bot token or a GitHub Actions "bypass" rule.
- **Alternative**: Version files use a `@VERSION@` placeholder that is only
  substituted in the OBS push (never committed back). The PKGBUILD in the repo
  always shows the latest released version because the commit happens at tag time
  before any protection applies (tags typically aren't branch-protected).

The simplest approach for a project without strict protection: use the default
`GITHUB_TOKEN` with `contents: write` permission in the workflow — no extra setup
needed when there are no required reviewers on `main`.

---

## GitHub Actions Workflow

File: `.github/workflows/release.yml`

Trigger: `on: push: tags: ['v*']`

Steps:
1. `actions/checkout` with `fetch-depth: 0`
2. Extract version: `VERSION=${GITHUB_REF_NAME#v}`
3. `sed -i "s/^Version:.*/Version: $VERSION/" packaging/agwkiss.spec`
4. `sed -i "s/^pkgver=.*/pkgver=$VERSION/" packaging/PKGBUILD`
5. Commit + push (`git commit -am "chore: release $VERSION" && git push`)
6. Write `~/.config/osc/oscrc` (see OBS auth below)
7. `pip install osc` (or install from distro in a container step)
8. `osc co home:CALLSIGN:agwkiss agwkiss`
9. Copy `packaging/agwkiss.spec` into the checked-out OBS package; sed-update the
   `<param name="revision">` in `obs/agwkiss/_service` to `v$VERSION`; copy both
10. `osc ci -m "release $VERSION"`

The `python-kiss3` OBS package is updated manually when `kiss3` itself releases —
it is not part of the agwkiss release workflow.

---

## OBS Authentication

`osc` is configured via `~/.config/osc/oscrc`. The workflow generates this file
from secrets:

```yaml
- name: Configure osc
  run: |
    mkdir -p ~/.config/osc
    cat > ~/.config/osc/oscrc <<EOF
    [general]
    apiurl = https://api.opensuse.org
    [https://api.opensuse.org]
    user = ${{ secrets.OBS_USERNAME }}
    pass = ${{ secrets.OBS_PASSWORD }}
    EOF
```

`OBS_PASSWORD` should be an OBS application password (not the account password),
generated at https://build.opensuse.org/user/tokens.

---

## Required GitHub Secrets

| Secret | Purpose |
|--------|---------|
| `OBS_USERNAME` | OBS account username |
| `OBS_PASSWORD` | OBS application token (not account password) |

---

## `_service` Files

**agwkiss/_service** — pulls source from GitHub. The `revision` param is updated
to the current tag by CI before pushing to OBS:

```xml
<services>
  <service name="obs_scm">
    <param name="url">https://github.com/CALLSIGN/agwkiss</param>
    <param name="scm">git</param>
    <param name="revision">v1.2.3</param>
    <param name="versionformat">@PARENT_TAG@</param>
    <param name="changesgenerate">enable</param>
  </service>
  <service name="tar"/>
  <service name="recompress"><param name="compression">gz</param></service>
  <service name="set_version"/>
</services>
```

**python-kiss3/_service** — pulls sdist from PyPI using the `obs_pypi` service.
No version is pinned here; OBS will always fetch the latest `kiss3` release from
PyPI on each rebuild. This is intentional: agwkiss only requires `>= 8.0.0` and
tracking latest reduces security exposure. If a breaking kiss3 release occurs,
add a `<param name="version">8.x.x</param>` to pin it.

```xml
<services>
  <service name="obs_pypi">
    <param name="package">kiss3</param>
    <param name="filename">python-kiss3</param>
  </service>
</services>
```

---

## PKGBUILD

Maintained at `packaging/PKGBUILD`. `pkgver` is updated by CI on each release.
The `license` field uses the current Arch SPDX convention:

```bash
pkgname=agwkiss
pkgver=1.2.3          # updated by release.yml
pkgrel=1
pkgdesc="AGWPE-to-KISS Translation Bridge"
arch=('any')
url="https://github.com/CALLSIGN/agwkiss"
license=('GPL-3.0-only')
depends=('python' 'python-pyserial' 'python-kiss3' 'python-pyham-ax25')
source=("$pkgname-$pkgver.tar.gz::$url/archive/v$pkgver.tar.gz")
```

`python-kiss3` and `python-pyham-ax25` are assumed to be available in AUR or
installed manually when using the PKGBUILD directly.

---

## RPM Spec Version Constraints

The `agwkiss.spec` `Requires:` section must reflect the version floors from
`requirements.txt`:

```spec
Requires: python3-kiss3 >= 8.0.0
Requires: python3-pyham-ax25 >= 1.0.0
Requires: python3-pyserial >= 3.5
```

`python-kiss3.spec` does not need a lower bound on its own version; it packages
whatever is current on PyPI at the time of the OBS build.

---

## Out of Scope

- Submitting `python-kiss3` to upstream distro repos (nice-to-have, separate effort)
- AUR submission (PKGBUILD is published; submission is a manual one-time step)
- macOS / Windows packages
- Automated Copr or PPA mirrors (can revisit if OBS proves insufficient)
