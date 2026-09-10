package ratelimit

import "net"

// NormalizeIP turns a peer address into a stable bucket key.
//
// It accepts anything the standard library hands us: "1.2.3.4:5678" from
// http.Request.RemoteAddr, "[::1]:5678" from a gRPC peer, or a bare address.
// The port is always stripped, otherwise every new TCP connection from the
// same host would get its own bucket and the limiter would do nothing.
// IPv4-mapped IPv6 addresses collapse onto their IPv4 form so that
// "::ffff:127.0.0.1" and "127.0.0.1" share one budget.
//
// It returns a nil IP when the address cannot be parsed.
func NormalizeIP(addr string) (net.IP, string) {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}

	ip := net.ParseIP(host)
	if ip == nil {
		return nil, ""
	}

	if v4 := ip.To4(); v4 != nil {
		return v4, v4.String()
	}

	return ip, ip.String()
}
