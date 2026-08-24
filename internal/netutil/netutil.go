// Package netutil provides network-access helpers shared by the frontends:
// a client-IP allowlist and a filtering listener that enforces it at Accept
// time, so all three listeners (AGWPE, KISS-TCP, API) share one
// implementation.
package netutil

import (
	"fmt"
	"log"
	"net"
	"net/netip"
	"strings"
)

// Allowlist is a set of CIDR prefixes a remote address must match. The zero
// value (no prefixes) allows everything, preserving pre-allowlist behavior.
type Allowlist struct {
	prefixes []netip.Prefix
}

// ParseAllowlist parses a comma-separated list of CIDRs. Bare IPs are treated
// as single-host prefixes (/32 or /128). Empty input yields an empty
// (allow-all) Allowlist.
func ParseAllowlist(s string) (Allowlist, error) {
	var a Allowlist
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "/") {
			p, err := netip.ParsePrefix(part)
			if err != nil {
				return Allowlist{}, fmt.Errorf("invalid CIDR %q: %w", part, err)
			}
			a.prefixes = append(a.prefixes, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(part)
		if err != nil {
			return Allowlist{}, fmt.Errorf("invalid address %q (want CIDR or IP): %w", part, err)
		}
		a.prefixes = append(a.prefixes, netip.PrefixFrom(ip, ip.BitLen()))
	}
	return a, nil
}

// Enabled reports whether the allowlist restricts anything (non-empty).
func (a Allowlist) Enabled() bool { return len(a.prefixes) > 0 }

// Allows reports whether addr's IP is permitted. A non-IP remote address
// (should not happen for TCP listeners) is rejected when the list is enabled.
func (a Allowlist) Allows(addr net.Addr) bool {
	if !a.Enabled() {
		return true
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		host = addr.String()
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range a.prefixes {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// WrapListener returns a listener whose Accept closes (and logs) connections
// from addresses the allowlist rejects. With an empty allowlist it returns
// the listener unchanged.
func WrapListener(ln net.Listener, a Allowlist, name string) net.Listener {
	if !a.Enabled() {
		return ln
	}
	return &allowlistListener{Listener: ln, allow: a, name: name}
}

type allowlistListener struct {
	net.Listener
	allow Allowlist
	name  string
}

func (l *allowlistListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.allow.Allows(c.RemoteAddr()) {
			return c, nil
		}
		log.Printf("%s: rejecting connection from %s (not in allowed_subnets)", l.name, c.RemoteAddr())
		c.Close()
	}
}
