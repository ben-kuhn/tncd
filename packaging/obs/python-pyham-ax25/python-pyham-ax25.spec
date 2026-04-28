Name:           python-pyham-ax25
Version:        1.0.3
Release:        1%{?dist}
Summary:        Python AX.25 packet radio protocol library
License:        Apache-2.0
URL:            https://pypi.org/project/pyham_ax25/
# https://files.pythonhosted.org/packages/51/75/3e206fb1583499137f36709f4aed440ebfa41866b495d1206c173c13fc3f/pyham_ax25-1.0.3.tar.gz
Source0:        pyham_ax25-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-pip
BuildRequires:  python3-setuptools

Requires:       python3

%description
Python implementation of the AX.25 packet radio protocol, providing
frame encode/decode for packet radio applications.

%prep
%autosetup -n pyham_ax25-%{version}

%build
python3 -m build --wheel --no-isolation

%install
python3 -m installer --destdir=%{buildroot} dist/*.whl

%files
%license LICENSE
%{python3_sitelib}/ax25/
%{python3_sitelib}/pyham_ax25-*.dist-info/

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 1.0.3-1
- Initial package
