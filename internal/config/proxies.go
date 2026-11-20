package config

import (
	"fmt"
	"net/netip"
	"strings"
)

const ipv4MappedBits = 96

// parseProxies turns the entries of TRUSTED_PROXIES into prefixes. A plain address becomes a prefix of one address.
func parseProxies(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))

	for i, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %d is empty, so remove the extra comma", i+1)
		}

		prefix, err := parseProxy(entry)
		if err != nil {
			return nil, fmt.Errorf("TRUSTED_PROXIES entry %q is not an IP address or a CIDR range", entry)
		}

		prefixes = append(prefixes, prefix)
	}

	return prefixes, nil
}

func parseProxy(entry string) (netip.Prefix, error) {
	if !strings.Contains(entry, "/") {
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("parse the address %w", err)
		}

		addr = addr.Unmap().WithZone("")

		return netip.PrefixFrom(addr, addr.BitLen()), nil
	}

	prefix, err := netip.ParsePrefix(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("parse the range %w", err)
	}

	// a peer is unmapped before the comparison, so a mapped IPv4 range has to be written as an IPv4 range
	if prefix.Addr().Is4In6() {
		if prefix.Bits() < ipv4MappedBits {
			return netip.Prefix{}, fmt.Errorf("the mapped range %q is wider than IPv4", entry)
		}

		prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-ipv4MappedBits)
	}

	return prefix.Masked(), nil
}

// TrustedPeer says whether the address of a peer is in the list of trusted proxies. The peer is an IP address, with or without a
// port. An empty list trusts nobody, and an address that does not parse is not trusted.
func TrustedPeer(proxies []netip.Prefix, peer string) bool {
	if len(proxies) == 0 {
		return false
	}

	addr, err := netip.ParseAddr(peer)
	if err != nil {
		addrPort, portErr := netip.ParseAddrPort(peer)
		if portErr != nil {
			return false
		}

		addr = addrPort.Addr()
	}

	addr = addr.Unmap().WithZone("")

	for _, prefix := range proxies {
		if prefix.Contains(addr) {
			return true
		}
	}

	return false
}
