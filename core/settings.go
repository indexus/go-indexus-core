package core

import (
	"net"
	"time"

	"github.com/indexus/go-indexus-core/encoding"
)

type Settings struct {
	id           []byte
	name         string
	ip           string
	ips          map[string]any
	port         int
	delay        time.Duration
	expiration   time.Duration
	delegation   int
	cacheBeta    float64
	queueMax     int
	cacheMax     int
	updateEta    float64 // skip probability for Update pulls (0..1)
	forwardRate  int     // max forwards per second (0 = unlimited)
	leafRedirect int     // dense leaf count threshold for hybrid redirect
}

func NewSettings(name string, port int, delay, expiration time.Duration, delegation int) (*Settings, error) {

	id, err := encoding.BASE64.Decode(name)
	if err != nil {
		return nil, err
	}

	return &Settings{
		id:           id,
		name:         name,
		ip:           "127.0.0.1",
		ips:          getPublicIPs(),
		port:         port,
		delay:        delay,
		expiration:   expiration,
		delegation:   delegation,
		cacheBeta:    1.0,
		queueMax:     10_000,
		cacheMax:     8_000,
		updateEta:    0.3,
		forwardRate:  200,
		leafRedirect: 64,
	}, nil
}

func (s *Settings) SetCacheBeta(beta float64) {
	if beta < 0 {
		beta = 0
	}
	s.cacheBeta = beta
}

func (s *Settings) SetQueueMax(max int) {
	s.queueMax = max
}

func (s *Settings) SetCacheMax(max int) {
	s.cacheMax = max
}

func (s *Settings) SetUpdateEta(eta float64) {
	if eta < 0 {
		eta = 0
	}
	if eta > 1 {
		eta = 1
	}
	s.updateEta = eta
}

func (s *Settings) SetForwardRate(r int) {
	s.forwardRate = r
}

func (s *Settings) SetLeafRedirect(threshold int) {
	s.leafRedirect = threshold
}

// SetAdvertise overrides the advertised IP set (e.g. 127.0.0.1 for local tests).
func (s *Settings) SetAdvertise(addrs ...string) {
	ips := make(map[string]any, len(addrs))
	for i, a := range addrs {
		if a == "" {
			continue
		}
		ips[a] = nil
		if i == 0 {
			s.ip = a
		}
	}
	if len(ips) > 0 {
		s.ips = ips
	}
}

// getPublicIPs retrieves all public IPv4 and IPv6 addresses and returns them in a map[string]any.
func getPublicIPs() map[string]any {
	ips := make(map[string]any)
	interfaces, err := net.Interfaces()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue // skip this interface on error
		}
		for _, addr := range addrs {
			// Get the IP address
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Skip nil IPs and loopback addresses
			if ip == nil || ip.IsLoopback() {
				continue
			}

			// Skip private IP addresses
			if isPrivateIP(ip) {
				continue
			}

			// Append public IPs to the list
			ips[ip.String()] = nil
		}
	}

	return ips
}

// Helper function to check if an IP is private
func isPrivateIP(ip net.IP) bool {
	return isPrivateIPv4(ip) || isPrivateIPv6(ip)
}

// Check for private IPv4 addresses
func isPrivateIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false // Not an IPv4 address
	}
	switch {
	case ip[0] == 10:
		return true // 10.0.0.0/8
	case ip[0] == 172 && ip[1]&0xf0 == 16:
		return true // 172.16.0.0/12
	case ip[0] == 192 && ip[1] == 168:
		return true // 192.168.0.0/16
	default:
		return false
	}
}

// Check for private IPv6 addresses
func isPrivateIPv6(ip net.IP) bool {
	ip = ip.To16()
	if ip == nil || ip.To4() != nil {
		return false // Not an IPv6 address
	}
	// Unique local addresses (fc00::/7)
	return ip[0]&0xfe == 0xfc
}
