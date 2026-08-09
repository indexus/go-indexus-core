package domain

import (
	"encoding/binary"
	"fmt"
)

// DecodeSetsBinary rebuilds a flat key→Abelian map from an EncodeSets body.
// propertyCount must match the encoder (p2p GetMultiple uses 4). Metrics are
// reconstructed with the same column layout: count, m2, m3*1e6, m4*1e6.
func DecodeSetsBinary(buf []byte, propertyCount int) (map[string]*Abelian, error) {
	if propertyCount < 1 {
		propertyCount = 4
	}
	out := make(map[string]*Abelian)
	offset := 0
	for offset < len(buf) {
		if offset+1+4+propertyCount > len(buf) {
			return nil, fmt.Errorf("sets binary: truncated header")
		}
		depthIndex := int(buf[offset])
		offset++
		size := int(binary.BigEndian.Uint32(buf[offset : offset+4]))
		offset += 4
		bitCounts := make([]int, propertyCount)
		for j := 0; j < propertyCount; j++ {
			bitCounts[j] = int(buf[offset])
			offset++
		}
		keyLen := depthIndex + 1
		keysBytes := size * keyLen
		if offset+keysBytes > len(buf) {
			return nil, fmt.Errorf("sets binary: truncated keys")
		}
		keys := make([]string, size)
		for i := 0; i < size; i++ {
			start := offset + i*keyLen
			keys[i] = string(buf[start : start+keyLen])
		}
		offset += keysBytes

		columns := make([][]int, propertyCount)
		for j := 0; j < propertyCount; j++ {
			values, consumed, err := decodePackedInts(buf, offset, size, bitCounts[j])
			if err != nil {
				return nil, err
			}
			columns[j] = values
			offset += consumed
		}

		for i := 0; i < size; i++ {
			count := 0
			if len(columns[0]) > i {
				count = columns[0][i]
			}
			metrics := make([]float64, 5)
			if propertyCount > 1 && len(columns[1]) > i {
				metrics[2] = float64(columns[1][i])
			}
			if propertyCount > 2 && len(columns[2]) > i {
				metrics[3] = float64(columns[2][i]) / 1_000_000
			}
			if propertyCount > 3 && len(columns[3]) > i {
				metrics[4] = float64(columns[3][i]) / 1_000_000
			}
			key := keys[i]
			next := NewAbelian(count, metrics)
			if existing, ok := out[key]; ok {
				existing.Sum(next)
			} else {
				out[key] = next
			}
		}
	}
	return out, nil
}

func decodePackedInts(buf []byte, offset, count, bitsPerValue int) ([]int, int, error) {
	if bitsPerValue <= 0 {
		return make([]int, count), 0, nil
	}
	totalBits := count * bitsPerValue
	bytesConsumed := (totalBits + 7) / 8
	if offset+bytesConsumed > len(buf) {
		return nil, 0, fmt.Errorf("sets binary: truncated packed ints")
	}
	values := make([]int, count)
	bitPos := 0
	for i := 0; i < count; i++ {
		var v uint64
		for b := 0; b < bitsPerValue; b++ {
			byteIdx := offset + (bitPos >> 3)
			bitInByte := bitPos & 7
			bit := (buf[byteIdx] >> (7 - bitInByte)) & 1
			v = (v << 1) | uint64(bit)
			bitPos++
		}
		values[i] = int(v)
	}
	return values, bytesConsumed, nil
}
