package mockidp

import (
	"fmt"
	"net"
	"strings"
)

// RequireLoopbackListen rejects every bind that is not a loopback host.
// An empty host, such as ":9443", listens on all interfaces and is rejected.
// Only "localhost" and IP addresses in the loopback range are accepted.
func RequireLoopbackListen(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ODYSSEIA_MOCK_IDP_ADDR: %w", err)
	}
	if loopbackHost(host) {
		return nil
	}
	return fmt.Errorf("ODYSSEIA_MOCK_IDP_ADDR %q must use a loopback host", addr)
}

func loopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return loopbackHost(remoteAddr)
	}
	return loopbackHost(host)
}

func loopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
