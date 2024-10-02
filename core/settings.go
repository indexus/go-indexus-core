package core

import (
	"net"
	"time"

	"gitlab.com/indexus/node/domain"
)

type Settings struct {
	id         []byte
	name       string
	ip         string
	ips        map[string]any
	port       int
	delay      time.Duration
	expiration time.Duration
	delegation int
	setLength  int
}

func NewSettings(name string, port int, delay, expiration time.Duration, delegation int, setLength int) (*Settings, error) {

	id, err := domain.DecodeName(name)
	if err != nil {
		return nil, err
	}

	return &Settings{
		id:         id,
		name:       name,
		ip:         "127.0.0.1",
		ips:        getPublicIPs(),
		port:       port,
		delay:      delay,
		expiration: expiration,
		delegation: delegation,
		setLength:  setLength,
	}, nil
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
