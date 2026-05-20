# Copyright 2026 Gentoo Authors
# Distributed under the terms of the GNU General Public License v3

EAPI=8

PYTHON_COMPAT=( python3_{11..13} )
inherit python-single-r1 systemd

DESCRIPTION="AGWPE-to-KISS translation bridge for amateur radio"
HOMEPAGE="https://tncd.dev"

MY_PV="${PV/_beta/-BETA}"
SRC_URI="https://github.com/ben-kuhn/${PN}/archive/v${MY_PV}.tar.gz -> ${P}.tar.gz"
S="${WORKDIR}/${PN}-${MY_PV}"

LICENSE="GPL-3"
SLOT="0"
KEYWORDS="~amd64 ~arm ~arm64 ~riscv ~x86"
IUSE="bluetooth"

REQUIRED_USE="${PYTHON_REQUIRED_USE}"

RDEPEND="
	${PYTHON_DEPS}
	$(python_gen_cond_dep '
		dev-python/pyserial[${PYTHON_USEDEP}]
		dev-python/kiss3[${PYTHON_USEDEP}]
		dev-python/pyham-ax25[${PYTHON_USEDEP}]
	')
	bluetooth? (
		$(python_gen_cond_dep '
			dev-python/dbus-python[${PYTHON_USEDEP}]
			dev-python/pygobject[${PYTHON_USEDEP}]
		')
		net-wireless/bluez
	)
"

src_install() {
	python_fix_shebang tncd.py

	exeinto /usr/bin
	newexe tncd.py tncd

	insinto /etc
	newins tncd.ini tncd.ini.example

	systemd_dounit tncd.service
	sed -i \
		-e "s|ExecStart=.*tncd[^-].*|ExecStart=/usr/bin/tncd -c /etc/tncd.ini|" \
		-e "/^WorkingDirectory=/d" \
		"${D}$(systemd_get_systemunitdir)/tncd.service" || die

	dodoc README.md
}

pkg_postinst() {
	elog "Copy /etc/tncd.ini.example to /etc/tncd.ini and edit for your setup."
	if use bluetooth; then
		elog ""
		elog "For Bluetooth support, ensure the service user is in the"
		elog "'bluetooth' group."
	fi
}
