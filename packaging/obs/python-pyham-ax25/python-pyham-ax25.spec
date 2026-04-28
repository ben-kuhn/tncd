Name:           python-pyham-ax25
Version:        1.0.3
Release:        1%{?dist}
Summary:        Python AX.25 packet radio protocol library
License:        MIT
URL:            https://pypi.org/project/pyham_ax25/
# https://files.pythonhosted.org/packages/51/75/3e206fb1583499137f36709f4aed440ebfa41866b495d1206c173c13fc3f/pyham_ax25-1.0.3.tar.gz
Source0:        pyham_ax25-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-pip
BuildRequires:  python3-flit-core

Requires:       python3

%description
AX.25 frame encoding, decoding, and socket support for packet radio
applications.

%prep
%autosetup -n pyham_ax25-%{version}

%build
python3 -m pip wheel --no-deps --no-build-isolation -w dist .

%install
python3 -m pip install --no-deps --no-build-isolation \
    --root=%{buildroot} --prefix=%{_prefix} dist/*.whl

%files
%license LICENSE
%{python3_sitelib}/ax25/
%{python3_sitelib}/pyham_ax25-%{version}.dist-info/

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 1.0.3-1
- Initial package
