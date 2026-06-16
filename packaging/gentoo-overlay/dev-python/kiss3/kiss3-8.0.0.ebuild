# Copyright 2026 Gentoo Authors
# Distributed under the terms of the Apache License v2.0

EAPI=8

DISTUTILS_USE_PEP517=setuptools
PYTHON_COMPAT=( python3_{11..13} )
inherit distutils-r1 pypi

DESCRIPTION="Pure-Python KISS and KISS-over-TCP for TNC devices"
HOMEPAGE="
	https://github.com/python-aprs/kiss3
	https://pypi.org/project/kiss3/
"

LICENSE="Apache-2.0"
SLOT="0"
KEYWORDS="~amd64 ~arm ~arm64 ~riscv ~x86"

RDEPEND="
	dev-python/attrs[${PYTHON_USEDEP}]
	>=dev-python/ax253-0.1.5[${PYTHON_USEDEP}]
	dev-python/bitarray[${PYTHON_USEDEP}]
	dev-python/importlib-metadata[${PYTHON_USEDEP}]
	>=dev-python/pyserial-asyncio-0.6[${PYTHON_USEDEP}]
"

distutils_enable_tests pytest
