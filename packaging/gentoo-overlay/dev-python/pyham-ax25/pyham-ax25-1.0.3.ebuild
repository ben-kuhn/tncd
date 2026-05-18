# Copyright 2026 Gentoo Authors
# Distributed under the terms of the MIT License

EAPI=8

DISTUTILS_USE_PEP517=setuptools
PYTHON_COMPAT=( python3_{11..13} )
inherit distutils-r1

# PyPI normalizes the name to pyham_ax25
MY_PN="pyham_ax25"
MY_P="${MY_PN}-${PV}"

DESCRIPTION="AX.25 frame encoding/decoding and socket support"
HOMEPAGE="
	https://github.com/mfncooper/pyham_ax25
	https://pypi.org/project/pyham-ax25/
"
SRC_URI="https://files.pythonhosted.org/packages/source/${MY_PN::1}/${MY_PN}/${MY_P}.tar.gz"
S="${WORKDIR}/${MY_P}"

LICENSE="MIT"
SLOT="0"
KEYWORDS="~amd64 ~arm ~arm64 ~riscv ~x86"

distutils_enable_tests pytest
