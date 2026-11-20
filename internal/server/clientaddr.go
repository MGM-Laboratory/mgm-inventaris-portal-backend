package server

import (
	"net/netip"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/config"
)

const (
	realIPHeader = "X-Real-IP"
	// ipv6PrefixBits is the part of an IPv6 address that counts as one client, because one subscriber holds a whole /64.
	ipv6PrefixBits = 64
	// unknownClient is the key of a peer whose address cannot be read, which no TCP listener produces.
	unknownClient = "unknown"
)

// clientAddr returns the key of the client of a request, as docs/api.md says. The service reads X-Real-IP only when the peer is in
// the list of trusted proxies. Otherwise it uses the address of the peer, and the header is ignored.
func clientAddr(c fiber.Ctx, trusted []netip.Prefix) string {
	return resolveClient(c.RequestCtx().RemoteAddr().String(), c.Get(realIPHeader), trusted)
}

func resolveClient(peer, realIP string, trusted []netip.Prefix) string {
	if config.TrustedPeer(trusted, peer) {
		// a header that does not parse falls back to the proxy, so a trusted proxy that sends a bad value never makes a free pass
		if addr, err := netip.ParseAddr(strings.TrimSpace(realIP)); err == nil {
			return clientKey(addr)
		}
	}

	if addrPort, err := netip.ParseAddrPort(peer); err == nil {
		return clientKey(addrPort.Addr())
	}

	if addr, err := netip.ParseAddr(peer); err == nil {
		return clientKey(addr)
	}

	return unknownClient
}

// clientKey is the address of an IPv4 client and the /64 prefix of an IPv6 client.
func clientKey(addr netip.Addr) string {
	addr = addr.Unmap().WithZone("")

	if addr.Is6() {
		prefix, err := addr.Prefix(ipv6PrefixBits)
		if err == nil {
			return prefix.String()
		}
	}

	return addr.String()
}
