Name:           python-ax253
Version:        0.1.5.post1
Release:        1%{?dist}
Summary:        Experimental pure Python AX.25 stack
License:        Apache-2.0
URL:            https://pypi.org/project/ax253/
# https://files.pythonhosted.org/packages/2e/94/f7400f96ed094e9b9daca9e9753e02eba4e3af347ec087b430f1dcd8e281/ax253-0.1.5.post1.tar.gz
Source0:        ax253-%{version}.tar.gz

BuildArch:      noarch
BuildRequires:  python3-devel
BuildRequires:  python3-pip
BuildRequires:  python3-setuptools
BuildRequires:  python3-setuptools_scm
BuildRequires:  python3-wheel

Requires:       python3
Requires:       python3-attrs
Requires:       python3-bitarray
Requires:       python3-importlib-metadata

%description
Experimental pure Python AX.25 stack providing frame encoding and
decoding for packet radio applications.

%prep
%autosetup -n ax253-%{version}

%build
# setuptools_scm cannot detect version without git; provide it explicitly
SETUPTOOLS_SCM_PRETEND_VERSION=%{version} \
    python3 -m pip wheel --no-deps --no-build-isolation -w dist .

%install
python3 -m pip install --no-deps --no-build-isolation \
    --root=%{buildroot} --prefix=%{_prefix} dist/*.whl

%files
%license LICENSE
%{python3_sitelib}/ax253/
%{python3_sitelib}/ax253-%{version}.dist-info/

%changelog
* Mon Apr 28 2026 tncd contributors <noreply@github.com> - 0.1.5.post1-1
- Initial package
