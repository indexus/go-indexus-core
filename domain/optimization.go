package domain

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"strings"
)

func decodeBase4ToBits(base4Str string) ([]bool, error) {
	bits := []bool{}

	for _, c := range base4Str {
		var value uint8
		switch c {
		case '0':
			value = 0
		case '1':
			value = 1
		case '2':
			value = 2
		case '3':
			value = 3
		default:
			return nil, fmt.Errorf("invalid base4 character: %c", c)
		}
		// Extract two bits
		bits = append(bits, (value&2)>>1 == 1)
		bits = append(bits, (value&1) == 1)
	}

	return bits, nil
}

// EncodeBits encodes a list of values into a compact byte slice using the specified number of bits per value.
func encodeBits(values []int, bitsPerValue int) []byte {
	var buffer uint64
	var bufferBits uint
	result := make([]byte, 0, (len(values)*bitsPerValue+7)/8)

	for _, value := range values {
		buffer = (buffer << bitsPerValue) | uint64(value)
		bufferBits += uint(bitsPerValue)

		for bufferBits >= 8 {
			byteValue := byte(buffer >> (bufferBits - 8))
			result = append(result, byteValue)
			bufferBits -= 8
			buffer &= (1 << bufferBits) - 1 // Mask remaining bits
		}
	}

	// Flush remaining bits
	for bufferBits > 0 {
		if bufferBits >= 8 {
			byteValue := byte(buffer >> (bufferBits - 8))
			result = append(result, byteValue)
			bufferBits -= 8
			buffer &= (1 << bufferBits) - 1
		} else {
			byteValue := byte(buffer << (8 - bufferBits))
			result = append(result, byteValue)
			bufferBits = 0
		}
	}

	return result
}

// GetMultiple retrieves and encodes data from the collection.
func (c *Collections) GetMultiple(col string, locations []string) []byte {
	var result []byte
	collection, exist := c.data[col]
	if !exist {
		return result
	}

	precision := 6
	properties := [4](func(*Abelian) int){
		func(a *Abelian) int {
			return a.count
		},
		func(a *Abelian) int {
			return int(a.metrics[2])
		},
		func(a *Abelian) int {
			return int(1_000_000 * a.metrics[3])
		},
		func(a *Abelian) int {
			return int(1_000_000 * a.metrics[4])
		},
	}

	type t struct {
		size    int
		bits    []int
		values  [][]int
		content []byte
	}

	data := make([]t, precision)
	for idx := range data {
		data[idx] = t{
			size:    0,
			bits:    make([]int, len(properties)),
			values:  make([][]int, 0),
			content: make([]byte, 0),
		}

		for v := 0; v < len(properties); v++ {
			data[idx].values = append(data[idx].values, make([]int, 0))
		}
	}

	for _, location := range locations {
		set, exist := collection.sets[location]
		if !exist {
			continue
		}

		for key, value := range set.list {
			idxColon := strings.IndexByte(key, ':')
			if idxColon >= 0 {
				key = key[:precision]
			}

			l := len(key) - 1

			data[l].size++
			data[l].content = append(data[l].content, []byte(key)...)

			for idx, property := range properties {
				p := property(value)
				data[l].values[idx] = append(data[l].values[idx], p)
				if p > data[l].bits[idx] {
					data[l].bits[idx] = p
				}
			}
		}
	}

	bitsNeeded := func(n int) int {
		if n <= 0 {
			return 0
		}
		return bits.Len(uint(n))
	}

	for i := 0; i < precision; i++ {
		if data[i].size == 0 {
			continue
		}

		header := []byte{}
		header = append(header, byte(i))

		// Encode size as four bytes (Big Endian)
		sizeBytes := make([]byte, 4)
		binary.BigEndian.PutUint32(sizeBytes, uint32(data[i].size))
		header = append(header, sizeBytes...)

		content := data[i].content

		for j := 0; j < len(properties); j++ {
			bitCount := bitsNeeded(data[i].bits[j])
			header = append(header, byte(bitCount))
			content = append(content, encodeBits(data[i].values[j], bitCount)...)
		}

		result = append(result, header...)
		result = append(result, content...)
	}

	return result
}

// decodeBits decodes a compact byte slice into a slice of integers using the specified number of bits per value.
// It ensures that exactly 'size' values are decoded.
func decodeBits(encoded []byte, bitsPerValue int, size int) ([]int, error) {
	if bitsPerValue <= 0 || bitsPerValue > 64 {
		return nil, errors.New("invalid bitsPerValue")
	}
	if size < 0 {
		return nil, errors.New("invalid size")
	}

	var buffer uint64
	var bufferBits uint
	result := make([]int, 0, size)

	for _, b := range encoded {
		buffer = (buffer << 8) | uint64(b)
		bufferBits += 8

		for bufferBits >= uint(bitsPerValue) && len(result) < size {
			value := int(buffer >> (bufferBits - uint(bitsPerValue)))
			result = append(result, value)
			bufferBits -= uint(bitsPerValue)
			buffer &= (1 << bufferBits) - 1 // Mask remaining bits
		}

		if len(result) == size {
			break
		}
	}

	if len(result) != size {
		return nil, fmt.Errorf("decoded values (%d) do not match expected size (%d)", len(result), size)
	}

	return result, nil
}

type Ab struct {
	Count   int
	Metrics []int
}

// DecodeMultiple decodes the byte slice produced by GetMultiple into a map of keys to Abelian objects.
func DecodeMultiple(encoded []byte) (map[string]*Ab, error) {
	result := make(map[string]*Ab)
	cursor := 0
	length := len(encoded)

	for cursor < length {
		// Check for at least 5 bytes: 1 (groupIndex) + 4 (size)
		if cursor+5 > length {
			return nil, errors.New("unexpected end of data while reading header")
		}

		// Read group index (i)
		groupIndex := int(encoded[cursor])
		cursor++

		// Read size (four bytes, big endian)
		size := int(binary.BigEndian.Uint32(encoded[cursor : cursor+4]))
		cursor += 4

		// Define the number of properties (as per GetMultiple, it's 2)
		numProperties := 4
		bitCounts := make([]int, numProperties)

		for j := 0; j < numProperties; j++ {
			bitCounts[j] = int(encoded[cursor])
			cursor++
		}

		// Calculate key length
		keyLength := groupIndex + 1

		// Read keys
		keysEnd := cursor + size*keyLength
		if keysEnd > length {
			return nil, errors.New("unexpected end of data while reading keys")
		}

		keys := make([]string, size)
		for i := 0; i < size; i++ {
			keyBytes := encoded[cursor : cursor+keyLength]
			keys[i] = string(keyBytes)
			cursor += keyLength
		}

		// Read and decode each property
		propertiesValues := make([][]int, numProperties)
		for j := 0; j < numProperties; j++ {
			bitsPerValue := bitCounts[j]
			// Calculate the number of bytes to read
			totalBits := size * bitsPerValue
			bytesToRead := (totalBits + 7) / 8
			if cursor+bytesToRead > length {
				return nil, errors.New("unexpected end of data while reading property values")
			}

			encodedValues := encoded[cursor : cursor+bytesToRead]
			cursor += bytesToRead

			values, err := decodeBits(encodedValues, bitsPerValue, size)
			if err != nil {
				return nil, fmt.Errorf("failed to decode bits for property %d: %v", j, err)
			}

			propertiesValues[j] = values
		}

		// Assign decoded values to keys
		for i, key := range keys {
			abelian, exists := result[key]
			if !exists {
				abelian = &Ab{}
				result[key] = abelian
			}

			// Assuming property 0 is Count and property 1 is Metrics[2]
			abelian.Count = propertiesValues[0][i]
			abelian.Metrics = []int{propertiesValues[1][i], propertiesValues[2][i], propertiesValues[3][i]}
		}
	}

	return result, nil
}
