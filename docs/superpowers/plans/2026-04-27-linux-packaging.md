# Linux Distribution Packaging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Produce native `.deb`, Fedora/openSUSE `.rpm`, and Arch `PKGBUILD` packages built and published automatically via OBS on each tagged GitHub release.

**Architecture:** Two OBS packages (`python-kiss3` + `agwkiss`) live under a single OBS project. A GitHub Actions workflow fires on `v*` tags, bumps version strings, and pushes updated spec/service files to OBS via `osc`. OBS builds all targets in parallel and publishes them to per-distro repos.

**Tech Stack:** OBS (`osc`), RPM spec, debhelper, PKGBUILD, GitHub Actions, `rpmlint`, `namcap`

---

## Pre-flight: service file paths

The repo's `agwkiss.service` and `agwkiss-rfcomm.service` use paths for a
manual install under `/opt/agwkiss`. When installed via a package manager, the
correct paths are:

| Field | Current (manual) | Packaged |
|-------|-----------------|---------|
| `ExecStart` (agwkiss) | `/usr/bin/python3 /opt/agwkiss/agwkiss.py -c /opt/agwkiss/agwkiss.ini` | `/usr/bin/agwkiss -c /etc/agwkiss.ini` |
| `ExecStart` (rfcomm) | `/usr/bin/python3 /opt/agwkiss/agwkiss-rfcomm ...` | `/usr/bin/agwkiss-rfcomm -c /etc/agwkiss.ini -m watch` |
| `WorkingDirectory` | `/opt/agwkiss` | *(remove)* |

The RPM spec and Debian rules patch these at install time using `sed`, so the
original service files in the repo remain valid for manual installs.

---

## File Map

| File | Action | Purpose |
|------|--------|---------|
| `pyproject.toml` | Create | Python package metadata; enables `pip install .` |
| `packaging/agwkiss.spec` | Create | RPM spec for Fedora + openSUSE |
| `packaging/debian/control` | Create | Debian package metadata + deps |
| `packaging/debian/rules` | Create | Debian build rules |
| `packaging/debian/changelog` | Create | Debian changelog (0.0.0 placeholder for `set_version`) |
| `packaging/debian/copyright` | Create | Debian copyright file |
| `packaging/PKGBUILD` | Create | Arch Linux package build |
| `packaging/obs/agwkiss/_service` | Create | OBS source service — pulls from GitHub tag |
| `packaging/obs/python-kiss3/_service` | Create | OBS source service — pulls from PyPI |
| `packaging/obs/python-kiss3/python-kiss3.spec` | Create | RPM spec for kiss3 |
| `.github/workflows/release.yml` | Create | CI — version bump + OBS push on `v*` tag |

---

## Task 1: Python package metadata

**Files:**
- Create: `pyproject.toml`

- [ ] **Write `pyproject.toml`**

```toml
[build-system]
requires = ["setuptools>=64"]
build-backend = "setuptools.build_meta"

[project]
name = "agwkiss"
version = "1.0.0"
description = "AGWPE-to-KISS Translation Bridge"
readme = "README.md"
license = {text = "GPL-3.0-or-later"}
requires-python = ">=3.8"
dependencies = [
    "kiss3>=8.0.0",
    "pyham-ax25>=1.0.0",
    "pyserial>=3.5",
]
# agwkiss has a proper main() function and is importable as a module.
# agwkiss-rfcomm uses a hyphen in its filename and cannot be an entry point;
# it is installed as a plain script.
scripts = ["agwkiss-rfcomm"]

[project.scripts]
agwkiss = "agwkiss:main"

[project.urls]
Homepage = "https://github.com/agwkit/agwkiss"

[tool.setuptools]
py-modules = ["agwkiss"]
```

- [ ] **Verify it installs cleanly**

```bash
pip install -e . --dry-run
```

Expected: no errors; `agwkiss` entry point listed; `agwkiss-rfcomm` listed as a script.

- [ ] **Commit**

```bash
git add pyproject.toml
git commit -m "feat(packaging): add pyproject.toml for package metadata"
```

---

## Task 2: RPM spec for agwkiss

**Files:**
- Create: `packaging/agwkiss.spec`

- [ ] **Create `packaging/` directory and write `agwkiss.spec`**

```bash
mkdir -p packaging
```

```spec
Name:           agwkiss
Version:        1.0.0
Release:        1%{?dist}
Summary:        AGWPE-to-KISS Translation Bridge
License:        GPL-3.0-or-later
URL:            https://github.com/agwkit/agwkiss
Source0:        %{name}-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel

Requires:       python3
Requires:       python3-kiss3 >= 8.0.0
Requires:       python3-pyham-ax25 >= 1.0.0
Requires:       python3-pyserial >= 3.5
Requires:       bluez

%description
A userspace bridge allowing AGWPE-compatible client applications to communicate
with KISS TNCs (serial or TCP). Implements full AX.25 connected mode including
SABM/UA handshake, I-frame sequencing, and RR acknowledgement.

%prep
%autosetup

%build
# Pure Python — nothing to compile.

%install
install -Dm755 agwkiss.py           %{buildroot}%{_bindir}/agwkiss
install -Dm755 agwkiss-rfcomm       %{buildroot}%{_bindir}/agwkiss-rfcomm
install -Dm644 agwkiss.ini          %{buildroot}%{_sysconfdir}/agwkiss.ini.example

# Create the systemd unit directory before writing to it.
install -d %{buildroot}%{_unitdir}

# Install service files with packaged paths (binary at /usr/bin, config at /etc).
sed \
    -e 's|ExecStart=.*agwkiss.*|ExecStart=/usr/bin/agwkiss -c /etc/agwkiss.ini|' \
    -e '/^WorkingDirectory=/d' \
    agwkiss.service > %{buildroot}%{_unitdir}/agwkiss.service

sed \
    -e 's|ExecStart=.*agwkiss-rfcomm.*|ExecStart=/usr/bin/agwkiss-rfcomm -c /etc/agwkiss.ini -m watch|' \
    -e '/^WorkingDirectory=/d' \
    agwkiss-rfcomm.service > %{buildroot}%{_unitdir}/agwkiss-rfcomm.service

%post
%systemd_post agwkiss.service agwkiss-rfcomm.service

%preun
%systemd_preun agwkiss.service agwkiss-rfcomm.service

%postun
%systemd_postun_with_restart agwkiss.service agwkiss-rfcomm.service

%files
%license COPYING
%{_bindir}/agwkiss
%{_bindir}/agwkiss-rfcomm
%config(noreplace) %{_sysconfdir}/agwkiss.ini.example
%{_unitdir}/agwkiss.service
%{_unitdir}/agwkiss-rfcomm.service

%changelog
* Mon Apr 27 2026 agwkiss contributors <noreply@github.com> - 1.0.0-1
- Initial package
```

- [ ] **Validate with rpmlint**

Install rpmlint if needed: `sudo dnf install rpmlint` or `sudo apt install rpmlint`

```bash
rpmlint packaging/agwkiss.spec
```

Expected: 0 errors. Warnings about a missing source tarball are acceptable when
building locally without the actual source.

- [ ] **Commit**

```bash
git add packaging/agwkiss.spec
git commit -m "feat(packaging): add RPM spec for agwkiss"
```

---

## Task 3: RPM spec for python-kiss3

**Files:**
- Create: `packaging/obs/python-kiss3/python-kiss3.spec`

This spec is used by OBS to build the `kiss3` PyPI package into an RPM. OBS
fetches the sdist via the `_service` file (Task 6); this spec builds it.

- [ ] **Download the kiss3 sdist to check its internal structure**

```bash
pip download --no-deps --no-binary :all: 'kiss3>=8.0.0' -d /tmp/kiss3-check
ls /tmp/kiss3-check
tar tzf /tmp/kiss3-check/kiss3-*.tar.gz | head -30
```

Note the top-level directory name (e.g. `kiss3-8.x.x`), whether it has `setup.py`
or `pyproject.toml`, and where the installed Python package directory lands
(typically `kiss/`). Adjust `%autosetup -n` and `%files` below if they differ
from `kiss3-<version>`.

- [ ] **Write `packaging/obs/python-kiss3/python-kiss3.spec`**

```bash
mkdir -p packaging/obs/python-kiss3
```

The `%build` and `%install` sections use direct `python3 setup.py` invocations
rather than `%py3_build`/`%py3_install` (Fedora-only) to remain compatible with
both Fedora and openSUSE targets in OBS.

```spec
Name:           python-kiss3
Version:        8.0.0
Release:        1%{?dist}
Summary:        Python KISS TNC protocol library
License:        GPL-3.0-or-later
URL:            https://pypi.org/project/kiss3/
Source0:        kiss3-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-setuptools

Requires:       python3
Requires:       python3-pyserial

%description
Python implementation of the KISS TNC protocol for packet radio applications.

%prep
%autosetup -n kiss3-%{version}

%build
python3 setup.py build

%install
python3 setup.py install --prefix=%{_prefix} --root=%{buildroot} --skip-build

%files
%license LICENSE
%{python3_sitelib}/kiss/
%{python3_sitelib}/kiss3-*.egg-info/

%changelog
* Mon Apr 27 2026 agwkiss contributors <noreply@github.com> - 8.0.0-1
- Initial package
```

> **Note:** If `kiss3` uses `pyproject.toml` without a `setup.py`, replace
> the `%build`/`%install` lines with:
> ```spec
> %build
> python3 -m build --no-isolation --wheel
> %install
> python3 -m installer --destdir=%{buildroot} dist/*.whl
> ```
> and add `python3-build` and `python3-installer` to `BuildRequires`.

- [ ] **Validate with rpmlint**

```bash
rpmlint packaging/obs/python-kiss3/python-kiss3.spec
```

Expected: 0 errors.

- [ ] **Commit**

```bash
git add packaging/obs/python-kiss3/python-kiss3.spec
git commit -m "feat(packaging): add RPM spec for python-kiss3"
```

---

## Task 4: Debian packaging

**Files:**
- Create: `packaging/debian/control`
- Create: `packaging/debian/rules`
- Create: `packaging/debian/changelog`
- Create: `packaging/debian/copyright`

> **Dependency note:** `python3-kiss3` is not in Debian/Ubuntu repos. It must
> be built from the `python-kiss3` OBS package and added to your local apt
> sources before running `dpkg-buildpackage` locally. OBS handles this
> automatically in its build environment via the project `_config`.

- [ ] **Write `packaging/debian/control`**

```
Source: agwkiss
Section: hamradio
Priority: optional
Maintainer: agwkiss contributors <noreply@github.com>
Build-Depends: debhelper-compat (= 13), python3
Standards-Version: 4.6.2
Rules-Requires-Root: no

Package: agwkiss
Architecture: all
Depends: ${misc:Depends}, python3, python3-serial, python3-pyham-ax25, python3-kiss3, bluez
Description: AGWPE-to-KISS Translation Bridge
 A userspace bridge allowing AGWPE-compatible client applications to
 communicate with KISS TNCs (serial or TCP). Implements full AX.25
 connected mode including SABM/UA handshake, I-frame sequencing, and
 RR acknowledgement.
```

- [ ] **Write `packaging/debian/rules`**

The `override_dh_auto_install` target manually installs scripts and service
files, patching the service `ExecStart` to use packaged paths.

```makefile
#!/usr/bin/make -f
%:
	dh $@

override_dh_auto_build:
	# pure Python — nothing to compile

override_dh_auto_install:
	install -Dm755 agwkiss.py     $(CURDIR)/debian/agwkiss/usr/bin/agwkiss
	install -Dm755 agwkiss-rfcomm $(CURDIR)/debian/agwkiss/usr/bin/agwkiss-rfcomm
	install -Dm644 agwkiss.ini    $(CURDIR)/debian/agwkiss/etc/agwkiss.ini.example
	# Patch service files for packaged install paths before installing.
	install -Dm644 /dev/null $(CURDIR)/debian/agwkiss/lib/systemd/system/agwkiss.service
	sed \
	    -e 's|ExecStart=.*agwkiss[^-].*|ExecStart=/usr/bin/agwkiss -c /etc/agwkiss.ini|' \
	    -e '/^WorkingDirectory=/d' \
	    agwkiss.service > $(CURDIR)/debian/agwkiss/lib/systemd/system/agwkiss.service
	install -Dm644 /dev/null $(CURDIR)/debian/agwkiss/lib/systemd/system/agwkiss-rfcomm.service
	sed \
	    -e 's|ExecStart=.*agwkiss-rfcomm.*|ExecStart=/usr/bin/agwkiss-rfcomm -c /etc/agwkiss.ini -m watch|' \
	    -e '/^WorkingDirectory=/d' \
	    agwkiss-rfcomm.service > $(CURDIR)/debian/agwkiss/lib/systemd/system/agwkiss-rfcomm.service
```

- [ ] **Make `rules` executable**

```bash
chmod +x packaging/debian/rules
```

- [ ] **Write `packaging/debian/changelog`**

The version `0.0.0` is a placeholder. OBS's `set_version` service replaces it
with the actual release version at build time.

```
agwkiss (0.0.0) UNRELEASED; urgency=medium

  * Initial release.

 -- agwkiss contributors <noreply@github.com>  Mon, 27 Apr 2026 00:00:00 +0000
```

- [ ] **Write `packaging/debian/copyright`**

```
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: agwkiss
Upstream-Contact: noreply@github.com
Source: https://github.com/agwkit/agwkiss

Files: *
Copyright: 2024 agwkiss contributors
License: GPL-3.0-or-later

License: GPL-3.0-or-later
 This program is free software: you can redistribute it and/or modify
 it under the terms of the GNU General Public License as published by
 the Free Software Foundation, either version 3 of the License, or
 (at your option) any later version.
 .
 On Debian systems, the complete text of the GPL version 3 can be found
 in '/usr/share/common-licenses/GPL-3'.
```

- [ ] **Validate changelog syntax**

```bash
# If dpkg-dev is installed (Debian/Ubuntu):
dpkg-parsechangelog -l packaging/debian/changelog
```

Expected: prints version, date, and maintainer with no errors.

- [ ] **Commit**

```bash
git add packaging/debian/
git commit -m "feat(packaging): add Debian packaging files"
```

---

## Task 5: PKGBUILD

**Files:**
- Create: `packaging/PKGBUILD`

- [ ] **Write `packaging/PKGBUILD`**

```bash
# Maintainer: Your Callsign <callsign@example.com>
pkgname=agwkiss
pkgver=1.0.0
pkgrel=1
pkgdesc="AGWPE-to-KISS Translation Bridge"
arch=('any')
url="https://github.com/agwkit/agwkiss"
license=('GPL-3.0-only')
depends=('python' 'python-pyserial' 'python-kiss3' 'python-pyham-ax25' 'bluez-utils')
source=("$pkgname-$pkgver.tar.gz::$url/archive/v$pkgver.tar.gz")
sha256sums=('SKIP')

package() {
    cd "$pkgname-$pkgver"
    install -Dm755 agwkiss.py             "$pkgdir/usr/bin/agwkiss"
    install -Dm755 agwkiss-rfcomm         "$pkgdir/usr/bin/agwkiss-rfcomm"
    install -Dm644 agwkiss.ini            "$pkgdir/etc/agwkiss.ini.example"
    # Install service files then patch ExecStart/WorkingDirectory for packaged paths.
    install -Dm644 agwkiss.service        "$pkgdir/usr/lib/systemd/system/agwkiss.service"
    sed -i \
        -e 's|ExecStart=.*agwkiss[^-].*|ExecStart=/usr/bin/agwkiss -c /etc/agwkiss.ini|' \
        -e '/^WorkingDirectory=/d' \
        "$pkgdir/usr/lib/systemd/system/agwkiss.service"
    install -Dm644 agwkiss-rfcomm.service "$pkgdir/usr/lib/systemd/system/agwkiss-rfcomm.service"
    sed -i \
        -e 's|ExecStart=.*agwkiss-rfcomm.*|ExecStart=/usr/bin/agwkiss-rfcomm -c /etc/agwkiss.ini -m watch|' \
        -e '/^WorkingDirectory=/d' \
        "$pkgdir/usr/lib/systemd/system/agwkiss-rfcomm.service"
}
```

> **SHA256 on AUR submission:** `sha256sums=('SKIP')` is for initial development.
> Before submitting to AUR, replace with the real checksum:
> ```bash
> cd packaging && makepkg -g
> ```
> This prints the correct `sha256sums` line; replace the `SKIP` entry with it.
> The CI workflow (Task 7) computes and substitutes the SHA256 automatically
> on each release.

- [ ] **Validate PKGBUILD syntax**

```bash
bash -n packaging/PKGBUILD
```

Expected: no output (no syntax errors).

If on an Arch system, also run:
```bash
cd packaging && namcap PKGBUILD
```

- [ ] **Commit**

```bash
git add packaging/PKGBUILD
git commit -m "feat(packaging): add Arch PKGBUILD"
```

---

## Task 6: OBS `_service` files

**Files:**
- Create: `packaging/obs/agwkiss/_service`
- Create: `packaging/obs/python-kiss3/_service`

- [ ] **Write `packaging/obs/agwkiss/_service`**

The `revision` value is updated by CI before each `osc ci`. The value here
should match the current release tag.

```bash
mkdir -p packaging/obs/agwkiss
```

```xml
<services>
  <service name="obs_scm">
    <param name="url">https://github.com/agwkit/agwkiss</param>
    <param name="scm">git</param>
    <param name="revision">v1.0.0</param>
    <param name="versionformat">@PARENT_TAG@</param>
    <param name="changesgenerate">enable</param>
  </service>
  <service name="tar"/>
  <service name="recompress">
    <param name="compression">gz</param>
  </service>
  <service name="set_version"/>
</services>
```

- [ ] **Write `packaging/obs/python-kiss3/_service`**

No version pin — tracks latest kiss3 from PyPI intentionally (agwkiss only
requires `>= 8.0.0`). Add `<param name="version">8.x.x</param>` to pin if a
breaking release occurs.

```bash
mkdir -p packaging/obs/python-kiss3
```

```xml
<services>
  <service name="obs_pypi">
    <param name="package">kiss3</param>
    <param name="filename">python-kiss3</param>
  </service>
</services>
```

- [ ] **Validate both files are well-formed XML**

```bash
python3 -c "
import xml.etree.ElementTree as ET
ET.parse('packaging/obs/agwkiss/_service')
ET.parse('packaging/obs/python-kiss3/_service')
print('OK')
"
```

Expected: `OK`

- [ ] **Commit**

```bash
git add packaging/obs/
git commit -m "feat(packaging): add OBS _service files"
```

---

## Task 7: GitHub Actions release workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Create the workflows directory**

```bash
mkdir -p .github/workflows
```

- [ ] **Write `.github/workflows/release.yml`**

```yaml
name: Release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest

    steps:
      - name: Checkout
        uses: actions/checkout@v4
        with:
          fetch-depth: 0
          token: ${{ secrets.GITHUB_TOKEN }}

      - name: Extract version
        run: echo "VERSION=${GITHUB_REF_NAME#v}" >> "$GITHUB_ENV"

      - name: Compute release tarball SHA256
        run: |
          URL="https://github.com/agwkit/agwkiss/archive/v$VERSION.tar.gz"
          SHA256=$(curl -sL "$URL" | sha256sum | cut -d' ' -f1)
          echo "SHA256=$SHA256" >> "$GITHUB_ENV"

      - name: Update version in packaging files
        run: |
          sed -i "s/^Version:.*/Version:        $VERSION/" packaging/agwkiss.spec
          sed -i "s/^pkgver=.*/pkgver=$VERSION/" packaging/PKGBUILD
          sed -i "s/sha256sums=.*/sha256sums=('$SHA256')/" packaging/PKGBUILD
          sed -i \
            "s|<param name=\"revision\">.*</param>|<param name=\"revision\">v$VERSION</param>|" \
            packaging/obs/agwkiss/_service

      - name: Commit version bump
        run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"
          git add packaging/agwkiss.spec packaging/PKGBUILD packaging/obs/agwkiss/_service
          git commit -m "chore: release $VERSION [skip ci]"
          git push origin HEAD:main

      - name: Install osc
        run: |
          sudo apt-get update -qq
          sudo apt-get install -y osc

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

      - name: Push agwkiss to OBS
        run: |
          osc co "home:${{ secrets.OBS_USERNAME }}:agwkiss" agwkiss
          cp packaging/agwkiss.spec \
             "home:${{ secrets.OBS_USERNAME }}:agwkiss/agwkiss/"
          cp packaging/obs/agwkiss/_service \
             "home:${{ secrets.OBS_USERNAME }}:agwkiss/agwkiss/"
          cd "home:${{ secrets.OBS_USERNAME }}:agwkiss/agwkiss"
          osc ci -m "release $VERSION"
```

> **Branch protection note:** The "Commit version bump" step pushes directly to
> `main`. If the repo has branch protection requiring PR reviews, this will fail
> with the default `GITHUB_TOKEN`. In that case, store a Personal Access Token
> with `contents: write` as `secrets.GH_PAT` and use it in the `token:` field
> of the `actions/checkout` step. The `[skip ci]` tag in the commit message
> prevents the push from re-triggering this workflow.

> **OBS project setup (one-time, manual — see Task 8):** The `osc co` step
> assumes the project and package already exist in OBS.

- [ ] **Validate the YAML is well-formed**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml')); print('OK')"
```

Expected: `OK`

- [ ] **Optionally lint the workflow with actionlint**

```bash
# Install: go install github.com/rhysd/actionlint/cmd/actionlint@latest
actionlint .github/workflows/release.yml
```

Expected: 0 errors. Warnings about `secrets.*` expressions and context
availability are expected in static analysis and safe to ignore.

- [ ] **Commit**

```bash
git add .github/workflows/release.yml
git commit -m "feat(packaging): add GitHub Actions release workflow"
```

---

## Task 8: OBS project setup (manual, one-time)

This task is performed in the OBS web UI at https://build.opensuse.org and
cannot be automated. Complete it before triggering the first release tag.

- [ ] **Create OBS project** `home:YOURCALLSIGN:agwkiss`
- [ ] **Configure project `_config`** — add repository dependencies so the
  project can resolve `python-pyham-ax25` from its existing OBS project.
  Contact the pyham-ax25 OBS maintainer for the correct project name.
- [ ] **Create `python-kiss3` package** — upload
  `packaging/obs/python-kiss3/_service` and
  `packaging/obs/python-kiss3/python-kiss3.spec`; trigger a build and verify it
  succeeds for Fedora and openSUSE targets before continuing
- [ ] **Create `agwkiss` package** — upload `packaging/obs/agwkiss/_service`
  and `packaging/agwkiss.spec`
- [ ] **Add GitHub repository secrets** — at Settings → Secrets → Actions, add
  `OBS_USERNAME` (your OBS account name) and `OBS_PASSWORD` (an OBS application
  token generated at https://build.opensuse.org/user/tokens — not your account
  password)
- [ ] **Test the full pipeline** — create a test tag and verify end-to-end:

```bash
git tag v1.0.0 && git push --tags
```

  - GitHub Actions workflow completes without errors
  - OBS picks up the new revision in `_service` and triggers a build
  - All three targets (Debian, Fedora, openSUSE) build successfully
  - Install the resulting `.deb` on Debian 12: `sudo dpkg -i agwkiss_*.deb && agwkiss --help`
  - Install the resulting `.rpm` on Fedora 40: `sudo rpm -i agwkiss-*.rpm && agwkiss --help`

---

## Validation Checklist

- [ ] `rpmlint packaging/agwkiss.spec` — 0 errors
- [ ] `rpmlint packaging/obs/python-kiss3/python-kiss3.spec` — 0 errors
- [ ] `bash -n packaging/PKGBUILD` — no syntax errors
- [ ] Both `_service` XML files parse cleanly (`python3 -c "import xml.etree.ElementTree as ET; ..."`)
- [ ] Workflow YAML parses cleanly
- [ ] OBS builds all three targets for `python-kiss3` without errors
- [ ] OBS builds all three targets for `agwkiss` without errors
- [ ] `agwkiss --help` works after installing the `.deb` on Debian 12
- [ ] `agwkiss --help` works after installing the `.rpm` on Fedora 40
- [ ] `systemctl status agwkiss` shows the correct `ExecStart` path after install
