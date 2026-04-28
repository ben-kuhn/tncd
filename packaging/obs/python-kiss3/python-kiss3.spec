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
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 8.0.0-1
- Initial package
