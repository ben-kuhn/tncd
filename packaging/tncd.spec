Name:           tncd
Version:        1.100~beta
Release:        1%{?dist}
Summary:        AGWPE-to-KISS Translation Bridge
License:        GPL-3.0-only
URL:            https://tncd.dev
Source0:        %{name}-%{version}.tar.gz

BuildRequires:  golang >= 1.25

# Fully static binary (CGO_ENABLED=0) — no runtime library dependencies.

%description
A userspace bridge allowing AGWPE-compatible client applications to communicate
with KISS TNCs (serial or TCP). Implements full AX.25 connected mode including
SABM/UA handshake, I-frame sequencing, and RR acknowledgement. This is the Go
port (tncd 2.0 line).

%global vertag 1.100-Beta

%prep
%autosetup

%build
export CGO_ENABLED=0
export GOFLAGS="-mod=readonly"
go build \
    -ldflags "-s -w -X github.com/ben-kuhn/tncd/v2/internal/version.Version=%{vertag}" \
    -o tncd \
    ./cmd/tncd

%install
install -Dm755 tncd              %{buildroot}%{_bindir}/tncd
install -Dm644 tncd.ini          %{buildroot}%{_sysconfdir}/tncd.ini.example

# Create the systemd unit directory before writing to it.
install -d %{buildroot}%{_unitdir}

# Install service file with packaged paths (binary at /usr/bin, config at /etc).
sed \
    -e 's|ExecStart=.*tncd[^-].*|ExecStart=/usr/bin/tncd -c /etc/tncd.ini|' \
    -e '/^WorkingDirectory=/d' \
    tncd.service > %{buildroot}%{_unitdir}/tncd.service

%post
%systemd_post tncd.service

%preun
%systemd_preun tncd.service

%postun
%systemd_postun_with_restart tncd.service

%files
%license COPYING
%{_bindir}/tncd
%config(noreplace) %{_sysconfdir}/tncd.ini.example
%{_unitdir}/tncd.service

%changelog
* Mon Jul 27 2026 KU0HN <ku0hn@ku0hn.radio> - 1.100~beta-1
- Read-only monitoring API (JSON + Server-Sent Events): /api/status,
  /api/connections, /api/events. New [api] section, disabled by default.
- Both new 2.0 frontends (KISS-over-TCP passthrough and the API) validated
  over the air using Direwolf as the local TNC.

* Fri Jul 24 2026 KU0HN <ku0hn@ku0hn.radio> - 1.99~beta-1
- KISS-over-TCP passthrough: KISS-native apps can share one TNC alongside
  AGWPE clients (new [kisstcp] section, disabled by default)
- Internal: generalized frontend subscriber bus; AGWPE monitor migrated onto it

* Thu Jul 23 2026 KU0HN <ku0hn@ku0hn.radio> - 1.98~beta-1
- Clearer error when a serial port is busy (in use / exclusively locked)
- KISS-entry probe retries and sends init on silence with an honest warning
  instead of falsely assuming KISS mode
- Defensive AX.25 SSID masking; codec test coverage and comment cleanup

* Thu Jul 17 2026 KU0HN <ku0hn@ku0hn.radio> - 1.97~beta-1
- Go port (tncd 2.0 beta line); static binary, no python3 dependency
- Packaging moves from fpm/Python to nfpm/Go cross-compilation

* Sun Jun 15 2026 KU0HN <ku0hn@ku0hn.radio> - 1.1-1
- Parallel Bluetooth port startup; AGWPE server no longer blocked by offline ports
- Fix SIGABRT crash from blocking dbus ConnectProfile thread (cross-thread heap corruption)
- Fix UnknownObject on props.Get for devices not yet visible to BlueZ
- Fix br-connection-busy noise; InProgress silently ignored in error handler

* Fri May 29 2026 KU0HN <ku0hn@ku0hn.radio> - 1.0-1
- First stable release; feature-complete AGWPE bridge and AX.25 v2.0 connected mode
- Kantronics KPC+ family (KPC-3+ / KPC-9612+) OTA-verified at 1200 baud

* Tue May 20 2026 KU0HN <ku0hn@ku0hn.radio> - 0.11.2-1
- Security hardening: AGWPE session ownership, client limits, idle timeout
- Retire tncd-rfcomm standalone service

* Mon Apr 28 2026 KU0HN <ku0hn@ku0hn.radio> - 0.1-1
- Initial package
