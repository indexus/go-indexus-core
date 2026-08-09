package domain

import (
	"encoding/binary"
	"fmt"
)

// Sets envelope magic: opt-in wrapper around EncodeSets that carries
// per-location owner redirects for client-managed (deep=false) navigation.
var setsEnvelopeMagic = [4]byte{'I', 'X', 'S', '1'}

// SetsRedirect names the peer a client should ask next for Location.
type SetsRedirect struct {
	Location string
	Name     string
	IP       string
	Port     int
}

// EncodeSetsEnvelope wraps a legacy EncodeSets body with redirect records.
// Clients that do not send envelope=1 never see this framing.
func EncodeSetsEnvelope(body []byte, redirects []SetsRedirect) []byte {
	n := len(redirects)
	// magic(4) + count(4) + redirects + body
	size := 8 + len(body)
	for _, r := range redirects {
		size += 2 + len(r.Location) + 2 + len(r.Name) + 2 + len(r.IP) + 2
	}
	out := make([]byte, 0, size)
	out = append(out, setsEnvelopeMagic[:]...)
	var countBuf [4]byte
	binary.BigEndian.PutUint32(countBuf[:], uint32(n))
	out = append(out, countBuf[:]...)
	for _, r := range redirects {
		out = appendLengthPrefixed(out, r.Location)
		out = appendLengthPrefixed(out, r.Name)
		out = appendLengthPrefixed(out, r.IP)
		var portBuf [2]byte
		binary.BigEndian.PutUint16(portBuf[:], uint16(r.Port))
		out = append(out, portBuf[:]...)
	}
	out = append(out, body...)
	return out
}

// DecodeSetsEnvelope splits an IXS1 payload into redirects and the inner
// EncodeSets body. ok=false when the buffer is not an envelope (legacy raw).
func DecodeSetsEnvelope(buf []byte) (redirects []SetsRedirect, body []byte, ok bool, err error) {
	if len(buf) < 8 || buf[0] != setsEnvelopeMagic[0] || buf[1] != setsEnvelopeMagic[1] ||
		buf[2] != setsEnvelopeMagic[2] || buf[3] != setsEnvelopeMagic[3] {
		return nil, buf, false, nil
	}
	n := int(binary.BigEndian.Uint32(buf[4:8]))
	offset := 8
	redirects = make([]SetsRedirect, 0, n)
	for i := 0; i < n; i++ {
		var loc, name, ip string
		loc, offset, err = readLengthPrefixed(buf, offset)
		if err != nil {
			return nil, nil, false, err
		}
		name, offset, err = readLengthPrefixed(buf, offset)
		if err != nil {
			return nil, nil, false, err
		}
		ip, offset, err = readLengthPrefixed(buf, offset)
		if err != nil {
			return nil, nil, false, err
		}
		if offset+2 > len(buf) {
			return nil, nil, false, fmt.Errorf("sets envelope: truncated port")
		}
		port := int(binary.BigEndian.Uint16(buf[offset : offset+2]))
		offset += 2
		redirects = append(redirects, SetsRedirect{
			Location: loc,
			Name:     name,
			IP:       ip,
			Port:     port,
		})
	}
	return redirects, buf[offset:], true, nil
}

// IsSetsEnvelope reports whether buf starts with the IXS1 magic.
func IsSetsEnvelope(buf []byte) bool {
	return len(buf) >= 4 &&
		buf[0] == setsEnvelopeMagic[0] &&
		buf[1] == setsEnvelopeMagic[1] &&
		buf[2] == setsEnvelopeMagic[2] &&
		buf[3] == setsEnvelopeMagic[3]
}

func appendLengthPrefixed(dst []byte, s string) []byte {
	var lenBuf [2]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(s)))
	dst = append(dst, lenBuf[:]...)
	return append(dst, s...)
}

func readLengthPrefixed(buf []byte, offset int) (string, int, error) {
	if offset+2 > len(buf) {
		return "", offset, fmt.Errorf("sets envelope: truncated length")
	}
	n := int(binary.BigEndian.Uint16(buf[offset : offset+2]))
	offset += 2
	if offset+n > len(buf) {
		return "", offset, fmt.Errorf("sets envelope: truncated string")
	}
	s := string(buf[offset : offset+n])
	return s, offset + n, nil
}
