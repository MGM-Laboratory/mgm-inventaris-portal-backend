package config

import "testing"

func TestTrustedPeer(t *testing.T) {
	proxies, err := parseProxies([]string{"192.0.2.10", "172.18.0.0/16", "2001:db8::1", "2001:db8:abcd::/48"})
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	tests := []struct {
		name    string
		proxies bool
		peer    string
		want    bool
	}{
		{"single IPv4", true, "192.0.2.10", true},
		{"another IPv4", true, "192.0.2.11", false},
		{"inside the range", true, "172.18.0.1", true},
		{"range edge", true, "172.18.255.255", true},
		{"outside the range", true, "172.19.0.1", false},
		{"peer with a port", true, "172.18.0.1:54321", true},
		{"mapped IPv4 peer", true, "::ffff:172.18.0.1", true},
		{"single IPv6", true, "2001:db8::1", true},
		{"IPv6 with a port", true, "[2001:db8::1]:8080", true},
		{"another IPv6", true, "2001:db8::2", false},
		{"inside the IPv6 range", true, "2001:db8:abcd:1::5", true},
		{"outside the IPv6 range", true, "2001:db8:abce::1", false},
		{"not an address", true, "proxy.example.test", false},
		{"empty peer", true, "", false},
		{"empty list", false, "172.18.0.1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			list := proxies
			if !tc.proxies {
				list = nil
			}

			if got := TrustedPeer(list, tc.peer); got != tc.want {
				t.Fatalf("TrustedPeer(%q) is %v, want %v", tc.peer, got, tc.want)
			}
		})
	}
}

func TestEmptySettingTrustsNobody(t *testing.T) {
	cfg, err := parse(baseEnv())
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	if TrustedPeer(cfg.TrustedProxies, "127.0.0.1") {
		t.Fatal("an empty TRUSTED_PROXIES trusted a peer")
	}
}

func TestMappedRangeBecomesIPv4(t *testing.T) {
	proxies, err := parseProxies([]string{"::ffff:10.1.0.0/112"})
	if err != nil {
		t.Fatalf("parse failed %v", err)
	}

	if got := proxies[0].String(); got != "10.1.0.0/16" {
		t.Fatalf("range is %s, want 10.1.0.0/16", got)
	}
}
