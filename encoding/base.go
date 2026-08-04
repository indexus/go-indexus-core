package encoding

import (
	"crypto/rand"
	"fmt"
	"math"

	"github.com/indexus/go-indexus-core/domain"
)

type Base struct {
	rootIdentifier string
	lengthConfig   int
	totalBits      int
	bitsPerChar    int
	encodeTable    string
	idLength       int
}

func NewBase(lengthConfig int, totalBits int) *Base {
	alphabet := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz-_"
	bitsPerChar := int(math.Log2(float64(lengthConfig)))
	encodeTable := alphabet[:lengthConfig]
	idLength := (totalBits + bitsPerChar - 1) / bitsPerChar

	return &Base{
		rootIdentifier: "@",
		lengthConfig:   lengthConfig,
		totalBits:      totalBits,
		bitsPerChar:    bitsPerChar,
		encodeTable:    encodeTable,
		idLength:       idLength,
	}
}

func (b *Base) Root() string {
	return b.rootIdentifier
}

func (b *Base) Precision() float64 {
	return float64(b.totalBits) / float64(b.bitsPerChar)
}

func (b *Base) Length() int {
	return b.lengthConfig
}

// IDLength is the width of an identifier in characters, and with it the depth of
// the trees keyed on identifiers. Encode always produces that width.
func (b *Base) IDLength() int {
	return b.idLength
}

func (b *Base) Packing() int {
	return b.bitsPerChar
}

func (b *Base) CharAt(idx int) string {
	if idx < 0 || idx >= len(b.encodeTable) {
		return ""
	}
	return string(b.encodeTable[idx])
}

func (b *Base) IndexOf(char rune) int {
	for i, c := range b.encodeTable {
		if c == char {
			return i
		}
	}
	return -1
}

func (b *Base) Parent(hash string) string {
	if hash == b.rootIdentifier {
		return ""
	}
	if len(hash)-1 == 0 {
		return b.rootIdentifier
	}
	return hash[:len(hash)-1]
}

func (b *Base) NewID() []byte {
	return make([]byte, b.idLength)
}

func (b *Base) RandomName() (string, error) {
	id := make([]byte, b.idLength)
	_, err := rand.Read(id)
	if err != nil {
		return "", err
	}
	return b.Encode(id), nil
}

// PreferNearTargets returns N distinct XOR-near keys around hot so multi-spawn
// peers land in adjacent slices instead of fighting over one neighborhood.
// keepBits of hot are preserved; the next bits encode the slot index.
func (b *Base) PreferNearTargets(hot string, n int) ([]string, error) {
	if n < 1 {
		n = 1
	}
	if n > 3 {
		n = 3
	}
	out := make([]string, n)
	var target []byte
	if hot != "" {
		if decoded, err := b.Decode(hot); err == nil {
			target = decoded
		}
	}
	// Preserve ~14 bits of the hot key, then branch on the next 2 bits so
	// N=2/3 peers take non-overlapping XOR slices around the hotspot.
	const keepBits = 14
	for i := 0; i < n; i++ {
		id := make([]byte, b.idLength)
		if _, err := rand.Read(id); err != nil {
			return nil, err
		}
		if len(target) > 0 {
			copyNearBits(id, target, keepBits)
			setBitRange(id, keepBits, 2, i)
		}
		out[i] = b.Encode(id)
	}
	return out, nil
}

func copyNearBits(dst, src []byte, keepBits int) {
	if keepBits < 0 {
		keepBits = 0
	}
	fullBytes := keepBits / 8
	rem := keepBits % 8
	for i := 0; i < fullBytes && i < len(dst) && i < len(src); i++ {
		dst[i] = src[i]
	}
	if rem > 0 && fullBytes < len(dst) && fullBytes < len(src) {
		mask := byte(0xFF << (8 - rem))
		dst[fullBytes] = (src[fullBytes] & mask) | (dst[fullBytes] & ^mask)
	}
}

// setBitRange writes the low width bits of value into dst starting at bitOffset
// (MSB-first within each byte, matching copyNearBits / RandomNameNear).
func setBitRange(dst []byte, bitOffset, width, value int) {
	for w := 0; w < width; w++ {
		bit := bitOffset + w
		byteIdx := bit / 8
		if byteIdx >= len(dst) {
			return
		}
		shift := 7 - (bit % 8)
		mask := byte(1 << shift)
		if (value>>(width-1-w))&1 == 1 {
			dst[byteIdx] |= mask
		} else {
			dst[byteIdx] &^= mask
		}
	}
}

// RandomNameNear returns a random ID that shares the first keepBits bits with
// target (XOR-near). Used when spawning nodes to relieve a hot owner.
func (b *Base) RandomNameNear(target []byte, keepBits int) (string, error) {
	id := make([]byte, b.idLength)
	_, err := rand.Read(id)
	if err != nil {
		return "", err
	}
	if len(target) == 0 {
		return b.Encode(id), nil
	}
	total := b.idLength * 8
	if keepBits < 0 {
		keepBits = 0
	}
	if keepBits > total {
		keepBits = total
	}
	if keepBits > len(target)*8 {
		keepBits = len(target) * 8
	}
	out := make([]byte, b.idLength)
	copy(out, id)
	fullBytes := keepBits / 8
	rem := keepBits % 8
	for i := 0; i < fullBytes && i < len(out) && i < len(target); i++ {
		out[i] = target[i]
	}
	if rem > 0 && fullBytes < len(out) && fullBytes < len(target) {
		mask := byte(0xFF << (8 - rem))
		out[fullBytes] = (target[fullBytes] & mask) | (out[fullBytes] & ^mask)
	}
	return b.Encode(out), nil
}

// Encode converts a byte slice into a base-N string of length b.idLength.
// It uses exactly b.idLength * b.bitsPerChar bits from src (padding with zero bits if needed).
func (b *Base) Encode(src []byte) string {
	totalBitsNeeded := b.idLength * b.bitsPerChar
	var value uint64
	var bitsInValue int

	// We collect b.idLength characters here
	encoded := make([]byte, 0, b.idLength)

	// We'll track how many bits we've produced
	bitsProduced := 0

	// Index in src
	byteIndex := 0
	srcLen := len(src)

	for bitsProduced < totalBitsNeeded {
		// If we don't have enough bits in 'value' to extract a character,
		// pull in the next byte (or pad with 0 if src is exhausted).
		if bitsInValue < b.bitsPerChar {
			if byteIndex < srcLen {
				value = (value << 8) | uint64(src[byteIndex])
				bitsInValue += 8
				byteIndex++
			} else {
				// No more bytes left; just shift in zero bits
				value <<= (b.bitsPerChar - bitsInValue)
				bitsInValue = b.bitsPerChar
			}
		}

		// Now extract b.bitsPerChar bits from 'value' to map to a character
		shift := bitsInValue - b.bitsPerChar
		index := (value >> shift) & uint64((1<<b.bitsPerChar)-1)
		bitsInValue -= b.bitsPerChar

		// Append the corresponding character
		encoded = append(encoded, b.encodeTable[index])
		bitsProduced += b.bitsPerChar
	}

	return string(encoded)
}

// Decode converts a base-N string (of any length) back into a byte slice.
// It will decode all characters in 's'. If the final bits don't align to a full byte,
// the final byte is padded on the right with zeros.
func (b *Base) Decode(s string) ([]byte, error) {
	var value uint64
	var bitsInValue int
	output := make([]byte, 0, (len(s)*b.bitsPerChar+7)/8)

	if s == b.rootIdentifier {
		return output, nil
	}

	for _, char := range s {
		idx := b.IndexOf(char)
		if idx < 0 {
			return nil, fmt.Errorf("invalid character '%c' in input", char)
		}

		// Shift in bitsPerChar bits
		value = (value << b.bitsPerChar) | uint64(idx)
		bitsInValue += b.bitsPerChar

		// For every full byte in 'value', pop it off
		for bitsInValue >= 8 {
			bitsInValue -= 8
			outByte := byte((value >> bitsInValue) & 0xFF)
			output = append(output, outByte)
		}
	}

	// If there are leftover bits, pad them (on the right) with zeros to make a full byte
	if bitsInValue > 0 {
		// Move leftover bits to the top of the byte and fill with zeros on the right
		lastByte := byte((value << (8 - bitsInValue)) & 0xFF)
		output = append(output, lastByte)
	}

	return output, nil
}

func MergeEncodings(encoder1 domain.Encoder, encoder2 domain.Encoder, encodedStr1 string, encodedStr2 string) ([]byte, error) {
	bytes1, err := encoder1.Decode(encodedStr1)
	if err != nil {
		return nil, fmt.Errorf("error converting first encoded string to bits: %v", err)
	}

	bytes2, err := encoder2.Decode(encodedStr2)
	if err != nil {
		return nil, fmt.Errorf("error converting second encoded string to bits: %v", err)
	}

	// Total number of bits in the first encoding
	length := 0

	if encodedStr1 != encoder1.Root() {
		length = len(encodedStr1) * encoder1.Packing()
	}

	// How many leftover bits in that last (partial) byte?
	left := length % 8

	// How many *complete* bytes come from the first encoding?
	byteIndex := length / 8

	var result []byte

	if left == 0 {
		// If the first encoded bits ended on a byte boundary,
		// just take the first `byteIndex` bytes of bytes1 and then
		// append from byteIndex onward in bytes2.
		if byteIndex > len(bytes1) {
			return nil, fmt.Errorf("byteIndex out of range in bytes1")
		}
		if byteIndex > len(bytes2) {
			return nil, fmt.Errorf("byteIndex out of range in bytes2")
		}
		result = append(result, bytes1[:byteIndex]...)
		result = append(result, bytes2[byteIndex:]...)
	} else {
		// We have a partial byte at the boundary that needs combining.
		if byteIndex >= len(bytes1) || byteIndex >= len(bytes2) {
			return nil, fmt.Errorf("byteIndex out of range for partial combination")
		}

		// Combine the 'left' bits of bytes1[byteIndex] with the top bits of bytes2[byteIndex].
		combinedByte := combineBytes(bytes1[byteIndex], bytes2[byteIndex], left)

		// Take the full bytes up to byteIndex from bytes1,
		// then add our newly combined byte, then append the remainder from bytes2.
		result = append(result, bytes1[:byteIndex]...)
		result = append(result, combinedByte)
		if byteIndex+1 < len(bytes2) {
			result = append(result, bytes2[byteIndex+1:]...)
		}
	}

	return result, nil
}

// combineBytes merges the top 'left' bits from byte1 with the bottom bits of byte2.
func combineBytes(byte1, byte2 byte, left int) byte {
	if left < 0 || left > 8 {
		return 0
	}
	// Mask out the top 'left' bits from byte1.
	mask := byte(0xFF) << (8 - left) // e.g. if left=3 => 11100000
	firstPart := byte1 & mask        // keep top bits only
	// Shift them down so they occupy the bottom portion
	firstPart >>= (8 - left)

	// Take the bottom (8 - left) bits from byte2 by shifting right
	secondPart := byte2 >> left

	// Reassemble into a new byte
	combined := (firstPart << (8 - left)) | secondPart
	return combined
}
