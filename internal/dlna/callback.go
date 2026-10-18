package dlna

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

type callbackTarget struct{ url, host, tlsName string }

// Compatibility: accept a short list of HTTP(S) callbacks,
// resolve names once, require every address to belong to the control point,
// and connect to the pinned IP while preserving HTTP Host and TLS SNI.
func callbackTargets(ctx context.Context, raw, peer string) ([]callbackTarget, error) {
	invalid := errors.New("invalid callback")
	if len(raw) > 4096 {
		return nil, invalid
	}
	address := net.ParseIP(peer)
	if address == nil || address.IsUnspecified() || address.IsMulticast() || address.IsLinkLocalUnicast() {
		return nil, invalid
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var targets []callbackTarget
	for rest := strings.TrimSpace(raw); rest != ""; {
		if len(targets) >= 4 || !strings.HasPrefix(rest, "<") {
			return nil, invalid
		}
		end := strings.IndexByte(rest, '>')
		if end < 0 {
			return nil, invalid
		}
		u, err := url.Parse(rest[1:end])
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
			return nil, invalid
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if value, err := net.LookupPort("tcp", port); err != nil || value <= 0 {
			return nil, invalid
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
		if err != nil || len(ips) == 0 {
			return nil, invalid
		}
		for _, ip := range ips {
			if !ip.IP.Equal(address) {
				return nil, invalid
			}
		}
		target := callbackTarget{host: u.Host, tlsName: u.Hostname()}
		u.Host = net.JoinHostPort(address.String(), port)
		target.url = u.String()
		targets = append(targets, target)
		rest = strings.TrimSpace(rest[end+1:])
	}
	if len(targets) == 0 {
		return nil, invalid
	}
	return targets, nil
}
