package encoding

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"math"
	"sort"

	"github.com/indexus/go-indexus-core/domain"
)

type weightedAnchor struct {
	key string
	w   int
}

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

// PreferNearInHalfSpace builds a node ID on target's side of the first XOR
// fork with self (legacy helper / tests). Prefer BestLoadSplitFork +
// PreferNearAtFork when item weights are available — an early fork alone can
// donate nearly the whole keyspace and empty the donor.
func (b *Base) PreferNearInHalfSpace(self, target []byte) (string, error) {
	if len(self) == 0 || len(target) == 0 {
		return "", fmt.Errorf("empty id")
	}
	n := minBytes(self, target)
	self, target = self[:n], target[:n]
	d := firstDiffBit(self, target)
	total := b.idLength * 8
	if d >= total {
		return b.RandomNameNear(target, total/2)
	}
	return b.PreferNearAtFork(self, target, d, bitAt(target, d))
}

// BestLoadSplitFork picks the XOR bit where flipping self's bit donates as
// close as possible to half the item weight via Range(self, prefer).
//
// Range predicate: with firstDiff(self,prefer)=b, every key with bitAt(key,b)
// == prefer's bit is donated. Count that — nothing else.
//
// Rejects splits that empty the donor (keep < ~20%).
func BestLoadSplitFork(self []byte, ownIDs [][]byte, weights []int) (forkBit int, sideBit byte, donateWeight, keepWeight int, ok bool) {
	cands := LoadSplitForkCandidates(self, ownIDs, weights)
	if len(cands) == 0 {
		return 0, 0, 0, 0, false
	}
	c := cands[0]
	return c.ForkBit, c.SideBit, c.Donate, c.Keep, true
}

// PreferNearAtFork: match self until forkBit, take sideBit, then follow
// locality bits (full leaf path when locality is a zone ID).
func (b *Base) PreferNearAtFork(self, locality []byte, forkBit int, sideBit byte) (string, error) {
	if len(self) == 0 {
		return "", fmt.Errorf("empty self")
	}
	total := b.idLength * 8
	if forkBit < 0 || forkBit >= total {
		return "", fmt.Errorf("fork bit %d out of range", forkBit)
	}
	id := make([]byte, b.idLength)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	copyNearBits(id, self, forkBit)
	setBit(id, forkBit, sideBit)
	if len(locality) > 0 {
		for bit := forkBit + 1; bit < total && bit < len(locality)*8; bit++ {
			setBit(id, bit, bitAt(locality, bit))
		}
	}
	return b.Encode(id), nil
}

// MaxPeerPrefixBits: PreferNear must first-diff peers before this bit
// (sharing ≥ this many leading bits ⇒ glued / empty joiner).
const MaxPeerPrefixBits = 24

// LoadSplitCandidate is one XOR embranchement that splits exclusive weight
// without emptying the donor.
type LoadSplitCandidate struct {
	ForkBit int
	SideBit byte
	Donate  int
	Keep    int
}

// LoadSplitForkCandidates lists every safe ~½ fork, best (closest to half) first.
// Only bits where exclusive leaves actually vary can appear (w∈(0,total)).
func LoadSplitForkCandidates(self []byte, ownIDs [][]byte, weights []int) []LoadSplitCandidate {
	if len(self) == 0 || len(ownIDs) == 0 || len(ownIDs) != len(weights) {
		return nil
	}
	total := 0
	for _, w := range weights {
		if w > 0 {
			total += w
		}
	}
	if total < 2 {
		return nil
	}
	target := total / 2
	idBits := len(self) * 8
	out := make([]LoadSplitCandidate, 0, 16)
	for b := 0; b < idBits; b++ {
		side := bitAt(self, b) ^ 1
		w := 0
		for i, id := range ownIDs {
			if len(id) == 0 {
				continue
			}
			if bitAt(id, b) == side {
				ww := weights[i]
				if ww < 1 {
					ww = 1
				}
				w += ww
			}
		}
		if w == 0 || w >= total {
			continue
		}
		remain := total - w
		if remain*5 < total {
			continue
		}
		out = append(out, LoadSplitCandidate{ForkBit: b, SideBit: side, Donate: w, Keep: remain})
	}
	sort.Slice(out, func(i, j int) bool {
		ei := out[i].Donate - target
		if ei < 0 {
			ei = -ei
		}
		ej := out[j].Donate - target
		if ej < 0 {
			ej = -ej
		}
		if ei != ej {
			return ei < ej
		}
		return out[i].ForkBit < out[j].ForkBit
	})
	return out
}

// PreferNearSplit: XOR embranchement on exclusive leaves (~½), PreferNear along
// a donate-side leaf. Rejects empty-donor, peer-glue, and claimed-zone steal.
func (b *Base) PreferNearSplit(
	self []byte,
	exclusiveIDs [][]byte,
	exclusiveW []int,
	claimedIDs [][]byte,
	knownPeers [][]byte,
) (name string, cand LoadSplitCandidate, err error) {
	cands := LoadSplitForkCandidates(self, exclusiveIDs, exclusiveW)
	if len(cands) == 0 {
		return "", LoadSplitCandidate{}, fmt.Errorf("no safe XOR embranchement on exclusive load")
	}
	total := 0
	for _, w := range exclusiveW {
		if w > 0 {
			total += w
		}
	}
	for _, c := range cands {
		for _, leaf := range sideLeaves(exclusiveIDs, exclusiveW, c.ForkBit, c.SideBit) {
			nm, e := b.PreferNearAtFork(self, leaf, c.ForkBit, c.SideBit)
			if e != nil || nm == "" {
				continue
			}
			prefer, e := b.Decode(nm)
			if e != nil {
				continue
			}
			if firstDiffBit(self, prefer) != c.ForkBit {
				continue
			}
			donate, keep, stolen := PreferNearScore(prefer, self, exclusiveIDs, exclusiveW, claimedIDs, knownPeers)
			if stolen > 0 {
				continue
			}
			if donate == 0 || donate >= total || keep*5 < total {
				continue
			}
			if len(knownPeers) > 0 && minLeadingDiff(prefer, knownPeers) >= MaxPeerPrefixBits {
				continue
			}
			return nm, c, nil
		}
	}
	return "", LoadSplitCandidate{}, fmt.Errorf("no PreferNear passed Voronoi / donor-keep gates")
}

type leafW struct {
	id []byte
	w  int
}

// sideLeaves: heaviest, weight-median, then XOR mid of the two heaviest.
func sideLeaves(ownIDs [][]byte, weights []int, forkBit int, sideBit byte) [][]byte {
	var side []leafW
	for i, id := range ownIDs {
		if len(id) == 0 || bitAt(id, forkBit) != sideBit {
			continue
		}
		w := 1
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		side = append(side, leafW{id, w})
	}
	if len(side) == 0 {
		return nil
	}
	sort.Slice(side, func(i, j int) bool {
		if side[i].w != side[j].w {
			return side[i].w > side[j].w
		}
		return bytes.Compare(side[i].id, side[j].id) < 0
	})
	out := [][]byte{side[0].id}
	if len(side) < 2 {
		return out
	}
	if mid := weightMedianID(side); mid != nil && !bytes.Equal(mid, side[0].id) {
		out = append(out, mid)
	}
	if pair := xorMidID(side[0].id, side[1].id); pair != nil &&
		!bytes.Equal(pair, side[0].id) &&
		(len(out) < 2 || !bytes.Equal(pair, out[1])) {
		out = append(out, pair)
	}
	return out
}

func weightMedianID(side []leafW) []byte {
	rows := append([]leafW{}, side...)
	sort.Slice(rows, func(i, j int) bool {
		return bytes.Compare(rows[i].id, rows[j].id) < 0
	})
	sum := 0
	for _, r := range rows {
		sum += r.w
	}
	mid, acc := sum/2, 0
	for _, r := range rows {
		acc += r.w
		if acc >= mid {
			return r.id
		}
	}
	return rows[len(rows)/2].id
}

func xorMidID(a, b []byte) []byte {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	d := firstDiffBit(a[:n], b[:n])
	out := make([]byte, len(a))
	copy(out, a)
	if d < n*8 {
		setBit(out, d, bitAt(b, d))
		for bit := d + 1; bit < n*8; bit++ {
			setBit(out, bit, bitAt(b, bit))
		}
	}
	return out
}

// PreferNearScore: exclusive donate/keep and claimed weight PreferNear would steal.
func PreferNearScore(
	prefer, self []byte,
	exclusiveIDs [][]byte, exclusiveW []int,
	claimedIDs [][]byte,
	knownPeers [][]byte,
) (donate, keep, stolen int) {
	total := 0
	for i, id := range exclusiveIDs {
		w := 1
		if i < len(exclusiveW) && exclusiveW[i] > 0 {
			w = exclusiveW[i]
		}
		total += w
		if bytes.Compare(xorBytesLocal(id, prefer), xorBytesLocal(id, self)) < 0 {
			donate += w
		}
	}
	keep = total - donate
	for _, id := range claimedIDs {
		if len(id) == 0 {
			continue
		}
		owner := self
		bestD := xorBytesLocal(id, self)
		for _, p := range knownPeers {
			d := xorBytesLocal(id, p)
			if bytes.Compare(d, bestD) < 0 {
				bestD = d
				owner = p
			}
		}
		if bytes.Compare(xorBytesLocal(id, prefer), xorBytesLocal(id, owner)) < 0 {
			stolen++
		}
	}
	return donate, keep, stolen
}

func heaviestOnForkSide(ownIDs [][]byte, weights []int, forkBit int, sideBit byte) []byte {
	best, bestW := -1, -1
	for i, id := range ownIDs {
		if len(id) == 0 {
			continue
		}
		if bitAt(id, forkBit) != sideBit {
			continue
		}
		w := 1
		if i < len(weights) && weights[i] > 0 {
			w = weights[i]
		}
		if w > bestW {
			best, bestW = i, w
		}
	}
	if best < 0 {
		return nil
	}
	return ownIDs[best]
}

func xorBytesLocal(a, b []byte) []byte {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	out := make([]byte, n)
	for i := 0; i < n; i++ {
		out[i] = a[i] ^ b[i]
	}
	return out
}

func bitAt(id []byte, i int) byte {
	if i < 0 || i/8 >= len(id) {
		return 0
	}
	return id[i/8] >> (7 - i%8) & 1
}

func setBit(id []byte, i int, v byte) {
	if i < 0 || i/8 >= len(id) {
		return
	}
	shift := 7 - (i % 8)
	mask := byte(1 << shift)
	if v != 0 {
		id[i/8] |= mask
	} else {
		id[i/8] &^= mask
	}
}

func minBytes(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	return n
}

// PreferNearTargets returns N distinct XOR-near keys around hot so multi-spawn
// peers land in adjacent slices instead of fighting over one neighborhood.
// keepBits of hot are preserved; the next bits encode the slot index.
// Prefer PreferNearInHalfSpace / load-split when owned zone weights exist.
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

// SplitLoadTargets picks N PreferNear IDs that partition weighted anchors into
// roughly equal load shares (½ for N=2, ⅓ for N=3). Each target is XOR-near the
// median-weight anchor of its share so ownership handoff spreads across peers
// instead of dumping everything onto one spawned neighbor.
func (b *Base) SplitLoadTargets(anchors []string, weights []int, n int) ([]string, error) {
	return b.SplitLoadTargetsAvoiding(anchors, weights, n, nil)
}

// SplitLoadTargetsAvoiding is SplitLoadTargets that steers PreferNear away from
// existing peer IDs (avoid). Spawning XOR-on-top of a live peer causes empty
// joiners and rebalance thrash — we keep load-split anchors but refuse names
// that share too many leading bits with a known node.
func (b *Base) SplitLoadTargetsAvoiding(anchors []string, weights []int, n int, avoid []string) ([]string, error) {
	if n < 1 {
		n = 1
	}
	if n > 3 {
		n = 3
	}
	if len(anchors) == 0 {
		return nil, fmt.Errorf("no anchors")
	}
	if len(weights) != len(anchors) {
		return nil, fmt.Errorf("weights len %d != anchors %d", len(weights), len(anchors))
	}

	rows := make([]weightedAnchor, 0, len(anchors))
	total := 0
	for i, a := range anchors {
		if a == "" {
			continue
		}
		w := weights[i]
		if w < 1 {
			w = 1
		}
		rows = append(rows, weightedAnchor{key: a, w: w})
		total += w
	}
	if len(rows) == 0 || total == 0 {
		return nil, fmt.Errorf("no weighted anchors")
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].key < rows[j].key })

	if n > len(rows) {
		n = len(rows)
	}
	share := total / n
	if share < 1 {
		share = 1
	}

	bins := make([][]weightedAnchor, 0, n)
	cur := make([]weightedAnchor, 0)
	curW := 0
	for _, r := range rows {
		cur = append(cur, r)
		curW += r.w
		if len(bins) < n-1 && curW >= share {
			bins = append(bins, cur)
			cur = nil
			curW = 0
		}
	}
	if len(cur) > 0 {
		bins = append(bins, cur)
	}
	for len(bins) < n {
		last := bins[len(bins)-1]
		bins = append(bins, []weightedAnchor{last[len(last)/2]})
	}
	if len(bins) > n {
		bins = bins[:n]
	}

	avoidIDs := decodePeerIDs(b, avoid)

	out := make([]string, 0, n)
	seen := make(map[string]struct{}, n)
	const keepBits = 24
	// Reject PreferNear that shares ≥ this many leading bits with a live peer
	// (firstDiffBit ≥ threshold ⇒ too close in XOR space).
	// 16 was too weak around MergeEncodings anchors (7vdM* swarm): joiners at
	// firstDiff 24–27 still landed empty and the donor kept spawning.
	const maxPeerPrefix = 24
	for _, bin := range bins {
		rep := binAnchorAwayFrom(b, bin, avoidIDs)
		decoded, err := b.Decode(rep)
		if err != nil {
			continue
		}
		name, err := b.randomNameNearAvoiding(decoded, keepBits, avoidIDs, maxPeerPrefix, seen)
		if err != nil {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("failed to build split targets")
	}
	for len(out) < n {
		out = append(out, out[len(out)-1])
	}
	return out[:n], nil
}

func decodePeerIDs(b *Base, names []string) [][]byte {
	out := make([][]byte, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		id, err := b.Decode(name)
		if err != nil || len(id) == 0 {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, id)
	}
	return out
}

// binAnchorAwayFrom keeps the weight-median for load split. Peer avoidance is
// applied when sampling RandomNameNear (not by picking a cold anchor).
func binAnchorAwayFrom(b *Base, bin []weightedAnchor, avoid [][]byte) string {
	_ = b
	_ = avoid
	return binMedianAnchor(bin)
}

// minLeadingDiff returns the smallest first-differing-bit index vs avoid IDs.
// Higher ⇒ more shared prefix bits with the closest peer (XOR-closer).
func minLeadingDiff(id []byte, avoid [][]byte) int {
	if len(avoid) == 0 {
		return 0
	}
	min := 1 << 20
	for _, peer := range avoid {
		d := firstDiffBit(id, peer)
		if d < min {
			min = d
		}
	}
	return min
}

func firstDiffBit(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	bit := 0
	for i := 0; i < n; i++ {
		x := a[i] ^ b[i]
		if x == 0 {
			bit += 8
			continue
		}
		for s := 7; s >= 0; s-- {
			if x&(1<<s) != 0 {
				return bit + (7 - s)
			}
		}
	}
	return bit
}

func (b *Base) randomNameNearAvoiding(target []byte, keepBits int, avoid [][]byte, maxPeerPrefix int, seen map[string]struct{}) (string, error) {
	tryKeep := keepBits
	for attempt := 0; attempt < 64; attempt++ {
		// Progressively loosen keepBits so we can escape a peer glued to the
		// load-split median without abandoning the bin entirely.
		if attempt > 0 && attempt%6 == 0 && tryKeep > 8 {
			tryKeep -= 4
		}
		name, err := b.RandomNameNear(target, tryKeep)
		if err != nil {
			return "", err
		}
		if _, dup := seen[name]; dup {
			continue
		}
		id, err := b.Decode(name)
		if err != nil {
			continue
		}
		// Accept when we do NOT share ≥ maxPeerPrefix bits with any live peer
		// (firstDiffBit < threshold ⇒ far enough in XOR space).
		if len(avoid) == 0 || minLeadingDiff(id, avoid) < maxPeerPrefix {
			return name, nil
		}
	}
	// Do NOT return a colliding name: empty joiners XOR-on-top of a live peer
	// claim exclusive-load in the pad metric but never receive zones via Range.
	return "", fmt.Errorf("no PreferNear far enough from known peers (maxPrefix=%d)", maxPeerPrefix)
}

func binMedianAnchor(bin []weightedAnchor) string {
	if len(bin) == 1 {
		return bin[0].key
	}
	sum := 0
	for _, r := range bin {
		sum += r.w
	}
	mid := sum / 2
	acc := 0
	for _, r := range bin {
		acc += r.w
		if acc >= mid {
			return r.key
		}
	}
	return bin[len(bin)/2].key
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
