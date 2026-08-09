package core

import (
	"fmt"
	"net"
	"os"
	"time"

	"github.com/indexus/go-indexus-core/encoding"
)

type Settings struct {
	id                []byte
	name              string
	ip                string
	ips               map[string]any
	port              int
	delay             time.Duration
	expiration        time.Duration
	delegation        int
	cacheBeta         float64
	queueMax          int
	peerQueueMax      int
	cacheMax          int
	updateEta      float64
	updateMaxPulls int
	updateBatch    bool
	forwardRate    int
	feedWorkers    int
	leafRedirect   int
	memRefusePct   float64
}

func NewSettings(name string, port int, delay, expiration time.Duration, delegation int) (*Settings, error) {

	id, err := encoding.BASE64.Decode(name)
	if err != nil {
		return nil, err
	}

	settings := &Settings{
		id:             id,
		name:           name,
		ip:             "127.0.0.1",
		ips:            getPublicIPs(),
		port:           port,
		delay:          delay,
		expiration:     expiration,
		delegation:     delegation,
		cacheBeta:      1.0,
		queueMax:       10_000,
		cacheMax:       8_000,
		updateEta:      0.3,
		updateMaxPulls: 32,
		updateBatch:    true,

		forwardRate:  2_000,
		feedWorkers:  32,
		leafRedirect: 64,

		memRefusePct: 60,
	}

	// Env overrides the -delegation flag when set (same pattern as queue/forward).
	if v := envInt("INDEXUS_DELEGATION", 0); v > 0 {
		settings.delegation = v
	}
	if v := envInt("INDEXUS_QUEUE_MAX", 0); v > 0 {
		settings.queueMax = v
	}
	if v := envInt("INDEXUS_FORWARD_RATE", -1); v >= 0 {
		settings.forwardRate = v
	}
	if v := envInt("INDEXUS_FEED_WORKERS", 0); v > 0 {
		settings.feedWorkers = v
	}
	if v := envInt("INDEXUS_UPDATE_MAX_PULLS", 0); v > 0 {
		settings.updateMaxPulls = v
	}
	if os.Getenv("INDEXUS_UPDATE_ETA") != "" {
		if v := envFloat("INDEXUS_UPDATE_ETA", -1); v >= 0 {
			settings.SetUpdateEta(v)
		}
	}
	if raw := os.Getenv("INDEXUS_UPDATE_BATCH"); raw != "" {
		settings.updateBatch = raw == "1" || raw == "true" || raw == "TRUE"
	}
	if v := envFloat("INDEXUS_MEM_REFUSE_PCT", 0); v > 0 {
		settings.memRefusePct = v
	}
	settings.peerQueueMax = peerCeiling(settings.queueMax)

	return settings, nil
}

func peerCeiling(queueMax int) int {
	return 4 * queueMax
}

func (s *Settings) SetCacheBeta(beta float64) {
	if beta < 0 {
		beta = 0
	}
	s.cacheBeta = beta
}

func (s *Settings) SetQueueMax(max int) {
	s.queueMax = max
	s.peerQueueMax = peerCeiling(max)
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

func (s *Settings) UpdateMaxPulls() int {
	if s == nil || s.updateMaxPulls <= 0 {
		return 32
	}
	return s.updateMaxPulls
}

func (s *Settings) UpdateBatch() bool {
	return s != nil && s.updateBatch
}

func (s *Settings) SetForwardRate(r int) {
	s.forwardRate = r
}

func (s *Settings) SetFeedWorkers(n int) {
	if n < 1 {
		n = 1
	}
	s.feedWorkers = n
}

func (s *Settings) SetLeafRedirect(threshold int) {
	s.leafRedirect = threshold
}

func (s *Settings) SetMemRefusePct(pct float64) {
	s.memRefusePct = pct
}

// DelegationSize is the soft item count at which a location range is owned
// and may split (-delegation / INDEXUS_DELEGATION).
func (s *Settings) DelegationSize() int {
	if s == nil {
		return 0
	}
	return s.delegation
}

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

func getPublicIPs() map[string]any {
	ips := make(map[string]any)
	interfaces, err := net.Interfaces()
	if err != nil {
		return map[string]any{"error": err.Error()}
	}

	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {

			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			if ip == nil || ip.IsLoopback() {
				continue
			}

			if isPrivateIP(ip) {
				continue
			}

			ips[ip.String()] = nil
		}
	}

	return ips
}

func isPrivateIP(ip net.IP) bool {
	return isPrivateIPv4(ip) || isPrivateIPv6(ip)
}

func isPrivateIPv4(ip net.IP) bool {
	ip = ip.To4()
	if ip == nil {
		return false
	}
	switch {
	case ip[0] == 10:
		return true
	case ip[0] == 172 && ip[1]&0xf0 == 16:
		return true
	case ip[0] == 192 && ip[1] == 168:
		return true
	default:
		return false
	}
}

func isPrivateIPv6(ip net.IP) bool {
	ip = ip.To16()
	if ip == nil || ip.To4() != nil {
		return false
	}

	return ip[0]&0xfe == 0xfc
}

func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var n float64
	if _, err := fmt.Sscanf(raw, "%f", &n); err != nil {
		return def
	}
	return n
}
