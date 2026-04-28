Name:           python-kiss3
Version:        8.0.0
Release:        1%{?dist}
Summary:        Python KISS TNC protocol library
License:        Apache-2.0
URL:            https://pypi.org/project/kiss3/
# https://files.pythonhosted.org/packages/8b/66/f2a20256f697ca1e55fe25778bfcdd884e0135af687f32d43001e47146ea/kiss3-8.0.0.tar.gz
Source0:        kiss3_%{version}.orig.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-pip
BuildRequires:  python3-setuptools
BuildRequires:  python3-setuptools_scm
BuildRequires:  python3-wheel

Requires:       python3
Requires:       python3-ax253 >= 0.1.5
Requires:       python3-attrs
Requires:       python3-bitarray
Requires:       python3-importlib-metadata
Requires:       python3-pyserial-asyncio

%description
A pure-Python implementation of serial KISS and KISS-over-TCP protocols
for communicating with TNC devices.

%prep
%autosetup -n kiss3-%{version}

%build
# setuptools_scm cannot detect version without git; provide it explicitly
SETUPTOOLS_SCM_PRETEND_VERSION=%{version} \
    python3 -m pip wheel --no-deps --no-build-isolation -w dist .

%install
python3 -m pip install --no-deps --no-build-isolation \
    --root=%{buildroot} --prefix=%{_prefix} dist/*.whl

%files
%license LICENSE
%{python3_sitelib}/kiss/
%{python3_sitelib}/kiss3-%{version}.dist-info/

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 8.0.0-1
- Initial package
