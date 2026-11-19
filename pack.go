package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
)

var typeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

const zeroOID = "0000000000000000000000000000000000000000"

// ---- pkt-line framing ----

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	return append([]byte(fmt.Sprintf("%04x", n)), payload...)
}

func pktText(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type packet struct {
	kind    string // data, flush, delim, response-end
	payload []byte
}

// decodePktLines parses a pkt-line stream; trailing raw bytes (a pack) are returned separately.
func decodePktLines(data []byte) (packets []packet, rest []byte, err error) {
	i := 0
	for i+4 <= len(data) {
		hdr := string(data[i : i+4])
		n, e := strconv.ParseUint(hdr, 16, 16)
		if e != nil {
			// not a pkt-line header: the remainder is raw (pack) data
			return packets, data[i:], nil
		}
		if n == 0 {
			packets = append(packets, packet{kind: "flush"})
			i += 4
			continue
		}
		if n == 1 {
			packets = append(packets, packet{kind: "delim"})
			i += 4
			continue
		}
		if n == 2 {
			packets = append(packets, packet{kind: "response-end"})
			i += 4
			continue
		}
		if n < 4 || i+int(n) > len(data) {
			return packets, data[i:], fmt.Errorf("truncated pkt-line")
		}
		packets = append(packets, packet{kind: "data", payload: data[i+4 : i+int(n)]})
		i += int(n)
	}
	return packets, data[i:], nil
}

// ---- packfile writing (full objects, version 2) ----

func packObjHeader(typ string, size int) []byte {
	code := typeCode[typ]
	var out []byte
	first := byte(code<<4) | byte(size&0x0f)
	rem := size >> 4
	for rem > 0 {
		out = append(out, first|0x80)
		first = byte(rem & 0x7f)
		rem >>= 7
	}
	out = append(out, first)
	return out
}

func zlibDeflate(data []byte) []byte {
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	w.Write(data)
	w.Close()
	return buf.Bytes()
}

func zlibInflate(data []byte) ([]byte, int, error) {
	cr := &countReader{data: data}
	r, err := zlib.NewReader(cr)
	if err != nil {
		return nil, 0, err
	}
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, 0, err
	}
	r.Close()
	return out, cr.n, nil
}

type countReader struct {
	data []byte
	n    int
}

func (c *countReader) Read(p []byte) (int, error) {
	if c.n >= len(c.data) {
		return 0, io.EOF
	}
	n := copy(p, c.data[c.n:])
	c.n += n
	return n, nil
}

// ReadByte lets compress/flate consume input byte-by-byte (no bufio read-ahead),
// so c.n is exactly the number of bytes the zlib stream used.
func (c *countReader) ReadByte() (byte, error) {
	if c.n >= len(c.data) {
		return 0, io.EOF
	}
	b := c.data[c.n]
	c.n++
	return b, nil
}

func writePackFull(objs []*Obj) []byte {
	var body []byte
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	binary.BigEndian.PutUint32(hdr[4:], 2)
	binary.BigEndian.PutUint32(hdr[8:], uint32(len(objs)))
	body = append(body, hdr...)
	for _, o := range objs {
		body = append(body, packObjHeader(o.Type, len(o.Data))...)
		body = append(body, zlibDeflate(o.Data)...)
	}
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// ---- packfile reading (full objects + ofs/ref deltas) ----

func readPackObjects(pack []byte, base func(string) *Obj) (map[string]*Obj, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("bad pack header")
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	type entry struct {
		offset   int
		typeCode int
		size     int
		data     []byte
		baseOff  int
		baseOID  string
	}
	var entries []entry
	byOffset := map[int]int{}
	off := 12
	for i := 0; i < count; i++ {
		if off >= trailerStart {
			return nil, fmt.Errorf("pack truncated")
		}
		e := entry{offset: off}
		b := pack[off]
		off++
		e.typeCode = int(b>>4) & 7
		size := int(b & 0x0f)
		shift := 4
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, fmt.Errorf("pack truncated header")
			}
			b = pack[off]
			off++
			size += int(b&0x7f) << shift
			shift += 7
		}
		e.size = size
		if e.typeCode == 6 {
			b = pack[off]
			off++
			val := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[off]
				off++
				val = ((val + 1) << 7) + int(b&0x7f)
			}
			e.baseOff = e.offset - val
		} else if e.typeCode == 7 {
			if off+20 > trailerStart {
				return nil, fmt.Errorf("pack truncated ref base")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		}
		if off > trailerStart {
			return nil, fmt.Errorf("pack overran trailer")
		}
		data, consumed, err := zlibInflate(pack[off:trailerStart])
		if err != nil {
			return nil, err
		}
		e.data = data
		off += consumed
		byOffset[e.offset] = i
		entries = append(entries, e)
	}
	resolved := make([]*Obj, count)
	var resolve func(i int) (*Obj, error)
	resolve = func(i int) (*Obj, error) {
		if resolved[i] != nil {
			return resolved[i], nil
		}
		e := entries[i]
		switch e.typeCode {
		case 1, 2, 3, 4:
			o := &Obj{Type: codeType[e.typeCode], Data: e.data}
			resolved[i] = o
			return o, nil
		case 6:
			bi, ok := byOffset[e.baseOff]
			if !ok {
				return nil, fmt.Errorf("ofs base not found")
			}
			bo, err := resolve(bi)
			if err != nil {
				return nil, err
			}
			out, err := applyDelta(bo.Data, e.data)
			if err != nil {
				return nil, err
			}
			o := &Obj{Type: bo.Type, Data: out}
			resolved[i] = o
			return o, nil
		case 7:
			var bo *Obj
			for j := range entries {
				if resolved[j] != nil && hashObj(resolved[j].Type, resolved[j].Data) == e.baseOID {
					bo = resolved[j]
					break
				}
			}
			if bo == nil && base != nil {
				bo = base(e.baseOID)
			}
			if bo == nil {
				return nil, fmt.Errorf("ref base not found")
			}
			out, err := applyDelta(bo.Data, e.data)
			if err != nil {
				return nil, err
			}
			o := &Obj{Type: bo.Type, Data: out}
			resolved[i] = o
			return o, nil
		}
		return nil, fmt.Errorf("unknown type code")
	}
	out := map[string]*Obj{}
	for i := range entries {
		o, err := resolve(i)
		if err != nil {
			return nil, err
		}
		out[hashObj(o.Type, o.Data)] = o
	}
	return out, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() int {
		val := 0
		shift := 0
		for {
			b := delta[off]
			off++
			val += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return val
	}
	srcSize := readSize()
	tgtSize := readSize()
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					cpOff += int(delta[off]) << (8 * bit)
					off++
				}
			}
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					cpSize += int(delta[off]) << (8 * (bit - 4))
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			n := int(op & 0x7f)
			out = append(out, delta[off:off+n]...)
			off += n
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	return out, nil
}

// ---- object graph traversal ----

// reachable collects every object reachable from roots; stop commits are included but not traversed.
func (r *Repo) reachable(roots []string, stop map[string]bool) map[string]bool {
	out := map[string]bool{}
	var walk func(oid string)
	walk = func(oid string) {
		if oid == "" || oid == zeroOID || out[oid] {
			return
		}
		o := r.getObj(oid)
		if o == nil {
			return
		}
		out[oid] = true
		switch o.Type {
		case "commit":
			if stop[oid] {
				return
			}
			c := parseCommit(o.Data)
			walk(c.Tree)
			for _, p := range c.Parents {
				walk(p)
			}
		case "tree":
			for _, e := range parseTree(o.Data) {
				if e.Mode != "160000" {
					walk(e.OID)
				}
			}
		case "tag":
			t := parseTagObj(o.Data)
			walk(t.Object)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

type tagInfo struct {
	Object string
	Type   string
	Tag    string
	Tagger string
}

func parseTagObj(data []byte) tagInfo {
	var t tagInfo
	s := string(data)
	idx := bytes.Index(data, []byte("\n\n"))
	head := s
	if idx >= 0 {
		head = s[:idx]
	}
	for _, line := range splitLines(head) {
		switch {
		case len(line) > 7 && line[:7] == "object ":
			t.Object = line[7:]
		case len(line) > 5 && line[:5] == "type ":
			t.Type = line[5:]
		case len(line) > 4 && line[:4] == "tag ":
			t.Tag = line[4:]
		case len(line) > 7 && line[:7] == "tagger ":
			t.Tagger = line[7:]
		}
	}
	return t
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// treeClosure adds a commit's tree and all nested trees/blobs to out.
func (r *Repo) treeClosure(treeOID string, out map[string]bool) {
	o := r.getObj(treeOID)
	if o == nil || o.Type != "tree" || out[treeOID] {
		return
	}
	out[treeOID] = true
	for _, e := range parseTree(o.Data) {
		if e.Mode == "160000" {
			continue
		}
		if e.Mode == "40000" {
			r.treeClosure(e.OID, out)
		} else {
			out[e.OID] = true
		}
	}
}