# Copyright 2026 Gentoo Authors
# Distributed under the terms of the Apache License v2.0

EAPI=8

DISTUTILS_USE_PEP517=setuptools
PYTHON_COMPAT=( python3_{11..13} )
inherit distutils-r1 pypi

MY_PV="${PV/_p/.post}"
MY_P="${PN}-${MY_PV}"

DESCRIPTION="Experimental pure-Python AX.25 stack"
HOMEPAGE="
	https://github.com/python-aprs/ax253
	https://pypi.org/project/ax253/
"
SRC_URI="$(pypi_sdist_url --no-normalize "${PN}" "${MY_PV}")"
S="${WORKDIR}/${MY_P}"

LICENSE="Apache-2.0"
SLOT="0"
KEYWORDS="~amd64 ~arm ~arm64 ~riscv ~x86"

RDEPEND="
	dev-python/attrs[${PYTHON_USEDEP}]
	dev-python/bitarray[${PYTHON_USEDEP}]
	dev-python/importlib-metadata[${PYTHON_USEDEP}]
"

distutils_enable_tests pytest
