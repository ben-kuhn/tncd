Name:           tncd
Version:        1.0.0
Release:        1%{?dist}
Summary:        AGWPE-to-KISS Translation Bridge
License:        GPL-3.0-or-later
URL:            https://github.com/agwkit/tncd
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
install -Dm755 tncd.py           %{buildroot}%{_bindir}/tncd
install -Dm755 tncd-rfcomm       %{buildroot}%{_bindir}/tncd-rfcomm
install -Dm644 tncd.ini          %{buildroot}%{_sysconfdir}/tncd.ini.example

# Create the systemd unit directory before writing to it.
install -d %{buildroot}%{_unitdir}

# Install service files with packaged paths (binary at /usr/bin, config at /etc).
sed \
    -e 's|ExecStart=.*tncd[^-].*|ExecStart=/usr/bin/tncd -c /etc/tncd.ini|' \
    -e '/^WorkingDirectory=/d' \
    tncd.service > %{buildroot}%{_unitdir}/tncd.service

sed \
    -e 's|ExecStart=.*tncd-rfcomm.*|ExecStart=/usr/bin/tncd-rfcomm -c /etc/tncd.ini -m watch|' \
    -e '/^WorkingDirectory=/d' \
    tncd-rfcomm.service > %{buildroot}%{_unitdir}/tncd-rfcomm.service

%post
%systemd_post tncd.service tncd-rfcomm.service

%preun
%systemd_preun tncd.service tncd-rfcomm.service

%postun
%systemd_postun_with_restart tncd.service tncd-rfcomm.service

%files
%license COPYING
%{_bindir}/tncd
%{_bindir}/tncd-rfcomm
%config(noreplace) %{_sysconfdir}/tncd.ini.example
%{_unitdir}/tncd.service
%{_unitdir}/tncd-rfcomm.service

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 1.0.0-1
- Initial package
