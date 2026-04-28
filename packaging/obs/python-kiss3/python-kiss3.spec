Name:           python-kiss3
Version:        8.0.0
Release:        1%{?dist}
Summary:        Python KISS TNC protocol library
License:        GPL-3.0-or-later
URL:            https://pypi.org/project/kiss3/
# https://files.pythonhosted.org/packages/8b/66/f2a20256f697ca1e55fe25778bfcdd884e0135af687f32d43001e47146ea/kiss3-8.0.0.tar.gz
Source0:        kiss3-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-pip
BuildRequires:  python3-setuptools

Requires:       python3
Requires:       python3-pyserial

%description
Python implementation of the KISS TNC protocol for packet radio applications.

%prep
%autosetup -n kiss3-%{version}

%build
python3 -m build --wheel --no-isolation

%install
python3 -m installer --destdir=%{buildroot} dist/*.whl

%files
%license LICENSE
%{python3_sitelib}/kiss/
%{python3_sitelib}/kiss3-*.dist-info/

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 8.0.0-1
- Initial package
