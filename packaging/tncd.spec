Name:           tncd
Version:        1.1
Release:        1%{?dist}
Summary:        AGWPE-to-KISS Translation Bridge
License:        GPL-3.0-or-later
URL:            https://tncd.dev
Source0:        %{name}-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel

Requires:       python3
Requires:       python3-kiss3 >= 8.0.0
%if 0%{?suse_version}
Requires:       python3-pyham_ax25 >= 1.0.0
%else
Requires:       python3-pyham-ax25 >= 1.0.0
%endif
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
install -Dm755 tncd.py           %{buildroot}%{_bindir}/tncd
install -Dm644 tncd.ini          %{buildroot}%{_sysconfdir}/tncd.ini.example

# Create the systemd unit directory before writing to it.
install -d %{buildroot}%{_unitdir}

# Install service files with packaged paths (binary at /usr/bin, config at /etc).
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
