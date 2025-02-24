package domain

type Bits struct {
	Content []byte
	Length  int
}

func (b *Bits) Parent() *Bits {
	if b.Length <= 0 {
		return b
	}

	newLength := b.Length - 1
	newContent := b.Content

	originalBytes := (b.Length + 7) / 8
	newBytes := (newLength + 7) / 8

	if newBytes < originalBytes {
		newContent = newContent[:len(newContent)-1]
	}

	return &Bits{
		Content: newContent,
		Length:  newLength,
	}
}

type BitWriter struct {
	bytes     []byte
	current   byte
	bitPos    uint8
	totalBits int
}

func NewBitWriter() *BitWriter {
	return &BitWriter{
		bytes:     []byte{},
		current:   0,
		bitPos:    0,
		totalBits: 0,
	}
}

func (bw *BitWriter) WriteBits(bits byte, n uint8) {
	for n > 0 {
		remaining := 8 - bw.bitPos
		toWrite := n
		if toWrite > remaining {
			toWrite = remaining
		}
		// Shift bits to align with the current byte position.
		shift := n - toWrite
		mask := byte((1 << toWrite) - 1)
		bw.current <<= toWrite
		bw.current |= (bits >> shift) & mask
		bw.bitPos += toWrite
		bw.totalBits += int(toWrite)
		n -= toWrite
		bits &= (1 << shift) - 1
		if bw.bitPos == 8 {
			bw.bytes = append(bw.bytes, bw.current)
			bw.current = 0
			bw.bitPos = 0
		}
	}
}

func (bw *BitWriter) Flush() {
	if bw.bitPos > 0 {
		bw.current <<= (8 - bw.bitPos)
		bw.bytes = append(bw.bytes, bw.current)
		bw.current = 0
		bw.bitPos = 0
	}
}

type BitReader struct {
	bytes     []byte
	current   byte
	bitPos    uint8
	totalBits int
	readBits  int
}

func NewBitReader(bytes []byte, totalBits int) *BitReader {
	return &BitReader{
		bytes:     bytes,
		current:   0,
		bitPos:    8,
		totalBits: totalBits,
		readBits:  0,
	}
}

func (br *BitReader) ReadBits(n uint8) (byte, bool) {
	if br.readBits+int(n) > br.totalBits {
		return 0, false
	}

	var result byte
	for i := uint8(0); i < n; i++ {
		if br.bitPos == 8 {
			if len(br.bytes) == 0 {
				return 0, false
			}
			br.current = br.bytes[0]
			br.bytes = br.bytes[1:]
			br.bitPos = 0
		}
		bit := (br.current >> (7 - br.bitPos)) & 1
		result = (result << 1) | bit
		br.bitPos++
		br.readBits++
	}
	return result, true
}
