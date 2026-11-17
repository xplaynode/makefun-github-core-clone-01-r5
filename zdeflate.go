package main

// zdeflate: a bit-exact reimplementation of the deflate compressor that
// Node's zlib exposes as deflateSync with default options (level 6,
// deflate_slow strategy selection, memLevel 8, default strategy, zlib
// wrapper, ANZAC++ string hash). It exists because the git pack format
// embeds compressed object streams whose exact bytes other tools compare;
// a merely-compatible compressor would produce different bytes.
//
// The implementation follows RFC 1951 and the published zlib algorithm:
// lazy match evaluation, hash chains over a 32 KiB window, dynamic/static/
// stored block choice by exact bit accounting, and canonical Huffman code
// construction. Inputs up to 65000 bytes are handled exactly (every git
// object in this forge is far smaller); larger inputs fall back to the
// standard library, which yields a valid stream but not identical bytes.

import (
	"bytes"
	"compress/zlib"
	"hash/adler32"
	"sync"
)

const (
	zMinMatch      = 3
	zMaxMatch      = 258
	zMinLookahead  = zMaxMatch + zMinMatch + 1 // 262
	zWSize         = 32768
	zWMask         = zWSize - 1
	zMaxDist       = zWSize - zMinLookahead // 32506
	zHashBits      = 15
	zHashSize      = 1 << zHashBits
	zHashMask      = zHashSize - 1
	zMemLevel      = 8
	zLitBufsize    = 1 << (zMemLevel + 6) // 16384
	zSymEnd        = (zLitBufsize - 1) * 3
	zTooFar        = 4096
	zLiterals      = 256
	zLCodes        = 286
	zDCodes        = 30
	zBlCodes       = 19
	zLengthCodes   = 29
	zHeapSize      = 819
	zMaxBits       = 15
	zMaxBlBits     = 7
	zEndBlock      = 256
	zBufSize       = 16
	zGoodMatch     = 8
	zMaxLazy       = 16
	zNiceMatch     = 128
	zMaxChain      = 128
	zMaxExactInput = 65000
)

var zExtraLbits = [zLengthCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 0}
var zExtraDbits = [zDCodes]int{0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 6, 7, 7, 8, 8, 9, 9, 10, 10, 11, 11, 12, 12, 13, 13}
var zExtraBlbits = [zBlCodes]int{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 2, 3, 7}
var zBlOrder = [zBlCodes]int{16, 17, 18, 0, 8, 7, 9, 6, 10, 5, 11, 4, 12, 3, 13, 2, 14, 1, 15}

type zCtData struct {
	freq uint16
	code uint16
	dad  uint16
	len  uint16
}

type zTreeDesc struct {
	dynTree    []zCtData
	staticTree []zCtData // nil for the bit-length tree
	extra      []int
	extraBase  int
	elems      int
	maxLength  int
	maxCode    int
}

var (
	zLengthCode  [zMaxMatch - zMinMatch + 1]uint8
	zBaseLength  [zLengthCodes]int
	zBaseDist    [zDCodes]int
	zDistCode    [512]uint8
	zStaticLtree []zCtData
	zStaticDtree []zCtData
)

func zBiReverse(code uint32, length int) uint16 {
	res := uint32(0)
	for {
		res |= code & 1
		code >>= 1
		res <<= 1
		length--
		if length <= 0 {
			break
		}
	}
	return uint16(res >> 1)
}

func init() {
	length := 0
	for code := 0; code < zLengthCodes-1; code++ {
		zBaseLength[code] = length
		for n := 0; n < (1 << zExtraLbits[code]); n++ {
			zLengthCode[length] = uint8(code)
			length++
		}
	}
	zLengthCode[length-1] = uint8(zLengthCodes - 1)

	dist := 0
	code := 0
	for ; code < 16; code++ {
		zBaseDist[code] = dist
		for n := 0; n < (1 << zExtraDbits[code]); n++ {
			zDistCode[dist] = uint8(code)
			dist++
		}
	}
	dist >>= 7
	for ; code < zDCodes; code++ {
		zBaseDist[code] = dist << 7
		for n := 0; n < (1 << (zExtraDbits[code] - 7)); n++ {
			zDistCode[256+dist] = uint8(code)
			dist++
		}
	}

	zStaticLtree = make([]zCtData, zLCodes+2)
	var blCount [zMaxBits + 1]uint16
	n := 0
	for ; n <= 143; n++ {
		zStaticLtree[n].len = 8
		blCount[8]++
	}
	for ; n <= 255; n++ {
		zStaticLtree[n].len = 9
		blCount[9]++
	}
	for ; n <= 279; n++ {
		zStaticLtree[n].len = 7
		blCount[7]++
	}
	for ; n <= 287; n++ {
		zStaticLtree[n].len = 8
		blCount[8]++
	}
	zGenCodes(zStaticLtree, zLCodes+1, &blCount)

	zStaticDtree = make([]zCtData, zDCodes)
	for n := 0; n < zDCodes; n++ {
		zStaticDtree[n].len = 5
		zStaticDtree[n].code = zBiReverse(uint32(n), 5)
	}
}

func zGenCodes(tree []zCtData, maxCode int, blCount *[zMaxBits + 1]uint16) {
	var nextCode [zMaxBits + 1]uint16
	code := 0
	for bits := 1; bits <= zMaxBits; bits++ {
		code = (code + int(blCount[bits-1])) << 1
		nextCode[bits] = uint16(code)
	}
	for n := 0; n <= maxCode; n++ {
		l := int(tree[n].len)
		if l == 0 {
			continue
		}
		tree[n].code = zBiReverse(uint32(nextCode[l]), l)
		nextCode[l]++
	}
}

func zDistCodeOf(dist int) int {
	if dist < 256 {
		return int(zDistCode[dist])
	}
	return int(zDistCode[256+(dist>>7)])
}

type zState struct {
	data   []byte
	window []byte
	prev   []uint16
	head   []uint16

	out     []byte
	biBuf   uint32
	biValid int

	dynLtree []zCtData
	dynDtree []zCtData
	blTree   []zCtData
	lDesc    zTreeDesc
	dDesc    zTreeDesc
	blDesc   zTreeDesc

	heap    []int
	heapLen int
	heapMax int
	depth   []uint8
	blCount [zMaxBits + 1]uint16

	optLen    uint64
	staticLen uint64
	symBuf    []byte
	matches   int

	strstart       int
	blockStart     int
	lookahead      int
	insert         int
	matchLength    int
	prevLength     int
	matchStart     int
	prevMatch      int
	matchAvailable bool
	insH           int
	filled         bool
}

func newZState(data []byte) *zState {
	s := &zState{
		data:     data,
		window:   make([]byte, 2*zWSize),
		prev:     make([]uint16, zWSize),
		head:     make([]uint16, zHashSize),
		dynLtree: make([]zCtData, zHeapSize),
		dynDtree: make([]zCtData, zHeapSize),
		blTree:   make([]zCtData, zHeapSize),
		heap:     make([]int, zHeapSize+2),
		depth:    make([]uint8, zHeapSize),
		symBuf:   make([]byte, 0, zSymEnd+3),
	}
	s.lDesc = zTreeDesc{dynTree: s.dynLtree, staticTree: zStaticLtree, extra: zExtraLbits[:], extraBase: zLiterals + 1, elems: zLCodes, maxLength: zMaxBits}
	s.dDesc = zTreeDesc{dynTree: s.dynDtree, staticTree: zStaticDtree, extra: zExtraDbits[:], extraBase: 0, elems: zDCodes, maxLength: zMaxBits}
	s.blDesc = zTreeDesc{dynTree: s.blTree, staticTree: nil, extra: zExtraBlbits[:], extraBase: 0, elems: zBlCodes, maxLength: zMaxBlBits}
	s.matchLength = zMinMatch - 1
	s.prevLength = zMinMatch - 1
	s.initBlock()
	return s
}

// reset prepares a pooled state for a new stream, keeping its buffers.
func (s *zState) reset(data []byte) {
	s.data = data
	clear(s.window)
	clear(s.prev)
	clear(s.head)
	if s.out != nil {
		s.out = s.out[:0]
	}
	s.biBuf = 0
	s.biValid = 0
	s.strstart = 0
	s.blockStart = 0
	s.lookahead = 0
	s.insert = 0
	s.matchLength = zMinMatch - 1
	s.prevLength = zMinMatch - 1
	s.matchStart = 0
	s.prevMatch = 0
	s.matchAvailable = false
	s.insH = 0
	s.filled = false
	s.initBlock()
}

func (s *zState) putByte(b byte) {
	s.out = append(s.out, b)
}

func (s *zState) putShort(w uint16) {
	s.out = append(s.out, byte(w), byte(w>>8))
}

func (s *zState) putShortMSB(w uint16) {
	s.out = append(s.out, byte(w>>8), byte(w))
}

func (s *zState) sendBits(value int, length int) {
	if s.biValid > zBufSize-length {
		s.biBuf |= uint32(uint16(value)) << uint(s.biValid)
		s.putShort(uint16(s.biBuf))
		s.biBuf = uint32(uint16(value)) >> uint(zBufSize-s.biValid)
		s.biValid += length - zBufSize
	} else {
		s.biBuf |= uint32(uint16(value)) << uint(s.biValid)
		s.biValid += length
	}
}

func (s *zState) sendCode(code int, tree []zCtData) {
	s.sendBits(int(tree[code].code), int(tree[code].len))
}

func (s *zState) biFlush() {
	if s.biValid == 16 {
		s.putShort(uint16(s.biBuf))
		s.biBuf = 0
		s.biValid = 0
	} else if s.biValid >= 8 {
		s.putByte(byte(s.biBuf))
		s.biBuf >>= 8
		s.biValid -= 8
	}
}

func (s *zState) biWindup() {
	if s.biValid > 8 {
		s.putShort(uint16(s.biBuf))
	} else if s.biValid > 0 {
		s.putByte(byte(s.biBuf))
	}
	s.biBuf = 0
	s.biValid = 0
}

func (s *zState) initBlock() {
	for n := 0; n < zLCodes; n++ {
		s.dynLtree[n].freq = 0
	}
	for n := 0; n < zDCodes; n++ {
		s.dynDtree[n].freq = 0
	}
	for n := 0; n < zBlCodes; n++ {
		s.blTree[n].freq = 0
	}
	s.dynLtree[zEndBlock].freq = 1
	s.optLen = 0
	s.staticLen = 0
	s.symBuf = s.symBuf[:0]
	s.matches = 0
}

func (s *zState) tallyLit(lit byte) bool {
	s.symBuf = append(s.symBuf, 0, 0, lit)
	s.dynLtree[lit].freq++
	return len(s.symBuf) == zSymEnd
}

func (s *zState) tallyDist(dist int, lenMinusMin int) bool {
	s.symBuf = append(s.symBuf, byte(dist), byte(dist>>8), byte(lenMinusMin))
	s.matches++
	d := dist - 1
	s.dynLtree[int(zLengthCode[lenMinusMin])+zLiterals+1].freq++
	s.dynDtree[zDistCodeOf(d)].freq++
	return len(s.symBuf) == zSymEnd
}

func (s *zState) insertString(str int) int {
	// ANZAC++ string hash: the 4-byte little-endian word at the string
	// start, multiplied and folded. Bucket mates share this 4-byte hash,
	// not necessarily their first three bytes.
	v := uint32(s.window[str]) | uint32(s.window[str+1])<<8 |
		uint32(s.window[str+2])<<16 | uint32(s.window[str+3])<<24
	s.insH = int(((v*66521 + 66521) >> 16) & zHashMask)
	head := int(s.head[s.insH])
	s.prev[str&zWMask] = uint16(head)
	s.head[s.insH] = uint16(str)
	return head
}

func (s *zState) fillWindow() {
	if s.filled {
		return
	}
	s.filled = true
	copy(s.window[s.strstart+s.lookahead:], s.data)
	s.lookahead = len(s.data)
	// The hash is computed directly from the window at insert time, and
	// insert is 0 at stream start, so there is no hash state to prime.
}

func (s *zState) longestMatch(curMatch int) int {
	chainLength := zMaxChain
	bestLen := s.prevLength
	niceMatch := zNiceMatch
	limit := 0
	if s.strstart > zMaxDist {
		limit = s.strstart - zMaxDist
	}
	if s.prevLength >= zGoodMatch {
		chainLength >>= 2
	}
	if niceMatch > s.lookahead {
		niceMatch = s.lookahead
	}
	scanEnd1 := s.window[s.strstart+bestLen-1]
	scanEnd := s.window[s.strstart+bestLen]
	for {
		match := curMatch
		if s.window[match+bestLen] == scanEnd &&
			s.window[match+bestLen-1] == scanEnd1 &&
			s.window[match] == s.window[s.strstart] &&
			s.window[match+1] == s.window[s.strstart+1] {
			l := 2
			for l < zMaxMatch && s.window[s.strstart+l] == s.window[match+l] {
				l++
			}
			if l > bestLen {
				s.matchStart = curMatch
				bestLen = l
				if l >= niceMatch {
					break
				}
				scanEnd1 = s.window[s.strstart+bestLen-1]
				scanEnd = s.window[s.strstart+bestLen]
			}
		}
		next := int(s.prev[curMatch&zWMask])
		chainLength--
		if next <= limit || chainLength == 0 {
			break
		}
		curMatch = next
	}
	if bestLen <= s.lookahead {
		return bestLen
	}
	return s.lookahead
}

func (s *zState) smaller(tree []zCtData, n int, m int) bool {
	return tree[n].freq < tree[m].freq ||
		(tree[n].freq == tree[m].freq && s.depth[n] <= s.depth[m])
}

func (s *zState) pqDownHeap(tree []zCtData, k int) {
	v := s.heap[k]
	j := k << 1
	for j <= s.heapLen {
		if j < s.heapLen && s.smaller(tree, s.heap[j+1], s.heap[j]) {
			j++
		}
		if s.smaller(tree, v, s.heap[j]) {
			break
		}
		s.heap[k] = s.heap[j]
		k = j
		j <<= 1
	}
	s.heap[k] = v
}

func (s *zState) genBitlen(desc *zTreeDesc) {
	tree := desc.dynTree
	maxCode := desc.maxCode
	stree := desc.staticTree
	extra := desc.extra
	base := desc.extraBase
	maxLength := desc.maxLength
	for bits := 0; bits <= zMaxBits; bits++ {
		s.blCount[bits] = 0
	}
	tree[s.heap[s.heapMax]].len = 0
	overflow := 0
	h := s.heapMax + 1
	for ; h < zHeapSize; h++ {
		n := s.heap[h]
		bits := int(tree[tree[n].dad].len) + 1
		if bits > maxLength {
			bits = maxLength
			overflow++
		}
		tree[n].len = uint16(bits)
		if n > maxCode {
			continue
		}
		s.blCount[bits]++
		xbits := 0
		if n >= base {
			xbits = extra[n-base]
		}
		f := tree[n].freq
		s.optLen += uint64(f) * uint64(bits+xbits)
		if stree != nil {
			s.staticLen += uint64(f) * uint64(int(stree[n].len)+xbits)
		}
	}
	if overflow == 0 {
		return
	}
	for overflow > 0 {
		bits := maxLength - 1
		for s.blCount[bits] == 0 {
			bits--
		}
		s.blCount[bits]--
		s.blCount[bits+1] += 2
		s.blCount[maxLength]--
		overflow -= 2
	}
	for bits := maxLength; bits != 0; bits-- {
		n := int(s.blCount[bits])
		for n != 0 {
			h--
			m := s.heap[h]
			if m > maxCode {
				continue
			}
			if int(tree[m].len) != bits {
				s.optLen += uint64(int64(bits)-int64(tree[m].len)) * uint64(tree[m].freq)
				tree[m].len = uint16(bits)
			}
			n--
		}
	}
}

func (s *zState) buildTree(desc *zTreeDesc) {
	tree := desc.dynTree
	stree := desc.staticTree
	elems := desc.elems
	s.heapLen = 0
	s.heapMax = zHeapSize
	maxCode := -1
	for n := 0; n < elems; n++ {
		if tree[n].freq != 0 {
			s.heapLen++
			s.heap[s.heapLen] = n
			maxCode = n
			s.depth[n] = 0
		} else {
			tree[n].len = 0
		}
	}
	for s.heapLen < 2 {
		node := 0
		if maxCode < 2 {
			maxCode++
			node = maxCode
		}
		s.heapLen++
		s.heap[s.heapLen] = node
		tree[node].freq = 1
		s.depth[node] = 0
		s.optLen--
		if stree != nil {
			s.staticLen -= uint64(stree[node].len)
		}
	}
	desc.maxCode = maxCode
	for n := s.heapLen / 2; n >= 1; n-- {
		s.pqDownHeap(tree, n)
	}
	node := elems
	for {
		n := s.heap[1]
		s.heap[1] = s.heap[s.heapLen]
		s.heapLen--
		s.pqDownHeap(tree, 1)
		m := s.heap[1]
		s.heapMax--
		s.heap[s.heapMax] = n
		s.heapMax--
		s.heap[s.heapMax] = m
		tree[node].freq = tree[n].freq + tree[m].freq
		if s.depth[n] >= s.depth[m] {
			s.depth[node] = s.depth[n] + 1
		} else {
			s.depth[node] = s.depth[m] + 1
		}
		tree[n].dad = uint16(node)
		tree[m].dad = uint16(node)
		s.heap[1] = node
		node++
		s.pqDownHeap(tree, 1)
		if s.heapLen < 2 {
			break
		}
	}
	s.heapMax--
	s.heap[s.heapMax] = s.heap[1]
	s.genBitlen(desc)
	zGenCodes(tree, maxCode, &s.blCount)
}

func (s *zState) scanTree(tree []zCtData, maxCode int) {
	prevlen := -1
	nextlen := int(tree[0].len)
	count := 0
	maxCount := 7
	minCount := 4
	if nextlen == 0 {
		maxCount = 138
		minCount = 3
	}
	tree[maxCode+1].len = 0xffff
	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].len)
		count++
		if count < maxCount && curlen == nextlen {
			continue
		} else if count < minCount {
			s.blTree[curlen].freq += uint16(count)
		} else if curlen != 0 {
			if curlen != prevlen {
				s.blTree[curlen].freq++
			}
			s.blTree[16].freq++
		} else if count <= 10 {
			s.blTree[17].freq++
		} else {
			s.blTree[18].freq++
		}
		count = 0
		prevlen = curlen
		if nextlen == 0 {
			maxCount = 138
			minCount = 3
		} else if curlen == nextlen {
			maxCount = 6
			minCount = 3
		} else {
			maxCount = 7
			minCount = 4
		}
	}
}

func (s *zState) sendTree(tree []zCtData, maxCode int) {
	prevlen := -1
	nextlen := int(tree[0].len)
	count := 0
	maxCount := 7
	minCount := 4
	if nextlen == 0 {
		maxCount = 138
		minCount = 3
	}
	for n := 0; n <= maxCode; n++ {
		curlen := nextlen
		nextlen = int(tree[n+1].len)
		count++
		if count < maxCount && curlen == nextlen {
			continue
		} else if count < minCount {
			for ; count != 0; count-- {
				s.sendCode(curlen, s.blTree)
			}
		} else if curlen != 0 {
			if curlen != prevlen {
				s.sendCode(curlen, s.blTree)
				count--
			}
			s.sendCode(16, s.blTree)
			s.sendBits(count-3, 2)
		} else if count <= 10 {
			s.sendCode(17, s.blTree)
			s.sendBits(count-3, 3)
		} else {
			s.sendCode(18, s.blTree)
			s.sendBits(count-11, 7)
		}
		count = 0
		prevlen = curlen
		if nextlen == 0 {
			maxCount = 138
			minCount = 3
		} else if curlen == nextlen {
			maxCount = 6
			minCount = 3
		} else {
			maxCount = 7
			minCount = 4
		}
	}
}

func (s *zState) buildBlTree() int {
	s.scanTree(s.dynLtree, s.lDesc.maxCode)
	s.scanTree(s.dynDtree, s.dDesc.maxCode)
	s.buildTree(&s.blDesc)
	maxBlIndex := zBlCodes - 1
	for ; maxBlIndex >= 3; maxBlIndex-- {
		if s.blTree[zBlOrder[maxBlIndex]].len != 0 {
			break
		}
	}
	s.optLen += 3*uint64(maxBlIndex+1) + 5 + 5 + 4
	return maxBlIndex
}

func (s *zState) sendAllTrees(lcodes int, dcodes int, blcodes int) {
	s.sendBits(lcodes-257, 5)
	s.sendBits(dcodes-1, 5)
	s.sendBits(blcodes-4, 4)
	for rank := 0; rank < blcodes; rank++ {
		s.sendBits(int(s.blTree[zBlOrder[rank]].len), 3)
	}
	s.sendTree(s.dynLtree, lcodes-1)
	s.sendTree(s.dynDtree, dcodes-1)
}

func (s *zState) storedBlock(buf []byte, storedLen int, last bool) {
	lastBit := 0
	if last {
		lastBit = 1
	}
	s.sendBits(lastBit, 3) // STORED_BLOCK << 1 + last
	s.biWindup()
	s.putShort(uint16(storedLen))
	s.putShort(uint16(^uint32(storedLen)))
	s.out = append(s.out, buf[:storedLen]...)
}

func (s *zState) compressBlock(ltree []zCtData, dtree []zCtData) {
	sx := 0
	for sx < len(s.symBuf) {
		dist := int(s.symBuf[sx]) | int(s.symBuf[sx+1])<<8
		lc := int(s.symBuf[sx+2])
		sx += 3
		if dist == 0 {
			s.sendCode(lc, ltree)
		} else {
			code := int(zLengthCode[lc])
			s.sendCode(code+zLiterals+1, ltree)
			extra := zExtraLbits[code]
			if extra != 0 {
				s.sendBits(lc-zBaseLength[code], extra)
			}
			dist--
			code = zDistCodeOf(dist)
			s.sendCode(code, dtree)
			extra = zExtraDbits[code]
			if extra != 0 {
				s.sendBits(dist-zBaseDist[code], extra)
			}
		}
	}
	s.sendCode(zEndBlock, ltree)
}

// flushBlock emits one deflate block for the symbols tallied so far.
func (s *zState) flushBlock(last bool) {
	storedLen := s.strstart - s.blockStart
	buf := s.window[s.blockStart:s.strstart]
	s.buildTree(&s.lDesc)
	s.buildTree(&s.dDesc)
	maxBlIndex := s.buildBlTree()
	optLenB := (s.optLen + 3 + 7) >> 3
	staticLenB := (s.staticLen + 3 + 7) >> 3
	if staticLenB <= optLenB {
		optLenB = staticLenB
	}
	lastBit := 0
	if last {
		lastBit = 1
	}
	if uint64(storedLen)+4 <= optLenB {
		s.storedBlock(buf, storedLen, last)
	} else if staticLenB == optLenB {
		s.sendBits((1<<1)+lastBit, 3) // STATIC_TREES
		s.compressBlock(zStaticLtree, zStaticDtree)
	} else {
		s.sendBits((2<<1)+lastBit, 3) // DYN_TREES
		s.sendAllTrees(s.lDesc.maxCode+1, s.dDesc.maxCode+1, maxBlIndex+1)
		s.compressBlock(s.dynLtree, s.dynDtree)
	}
	s.initBlock()
	if last {
		s.biWindup()
	}
	s.blockStart = s.strstart
}

func (s *zState) deflateSlowFinish() {
	for {
		if s.lookahead < zMinLookahead {
			s.fillWindow()
			if s.lookahead == 0 {
				break
			}
		}
		hashHead := 0
		if s.lookahead >= zMinMatch {
			hashHead = s.insertString(s.strstart)
		}
		s.prevLength = s.matchLength
		s.prevMatch = s.matchStart
		s.matchLength = zMinMatch - 1
		if hashHead != 0 && s.prevLength < zMaxLazy && s.strstart-hashHead <= zMaxDist {
			s.matchLength = s.longestMatch(hashHead)
			if s.matchLength <= 5 && s.matchLength == zMinMatch && s.strstart-s.matchStart > zTooFar {
				s.matchLength = zMinMatch - 1
			}
		}
		if s.prevLength >= zMinMatch && s.matchLength <= s.prevLength {
			maxInsert := s.strstart + s.lookahead - zMinMatch
			bflush := s.tallyDist(s.strstart-1-s.prevMatch, s.prevLength-zMinMatch)
			s.lookahead -= s.prevLength - 1
			s.prevLength -= 2
			for {
				s.strstart++
				if s.strstart <= maxInsert {
					s.insertString(s.strstart)
				}
				s.prevLength--
				if s.prevLength == 0 {
					break
				}
			}
			s.matchAvailable = false
			s.matchLength = zMinMatch - 1
			s.strstart++
			if bflush {
				s.flushBlock(false)
			}
		} else if s.matchAvailable {
			bflush := s.tallyLit(s.window[s.strstart-1])
			if bflush {
				s.flushBlock(false)
			}
			s.strstart++
			s.lookahead--
		} else {
			s.matchAvailable = true
			s.strstart++
			s.lookahead--
		}
	}
	if s.matchAvailable {
		s.tallyLit(s.window[s.strstart-1])
		s.matchAvailable = false
	}
	s.flushBlock(true)
}

// zStatePool recycles compressor state: the window, hash tables and Huffman
// work areas are large and would otherwise be allocated per object.
var zStatePool = sync.Pool{New: func() any { return newZState(nil) }}

// zdeflate6 compresses data exactly like Node's zlib.deflateSync(data) with
// default options. Inputs over zMaxExactInput use the standard library.
func zdeflate6(data []byte) []byte {
	if len(data) > zMaxExactInput {
		var buf bytes.Buffer
		w, _ := zlib.NewWriterLevel(&buf, 6)
		w.Write(data)
		w.Close()
		return buf.Bytes()
	}
	s := zStatePool.Get().(*zState)
	s.reset(data)
	s.out = append(s.out, 0x78, 0x9c)
	s.deflateSlowFinish()
	sum := adler32.Checksum(data)
	s.putShortMSB(uint16(sum >> 16))
	s.putShortMSB(uint16(sum))
	out := s.out
	s.out = nil
	zStatePool.Put(s)
	return out
}
