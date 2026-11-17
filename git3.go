package main

// Receive-pack: parse update commands and the pushed pack, store the
// objects, validate each ref update independently (fast-forward rules),
// and answer with a report-status stream.

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type refCommand struct {
	oldOID string
	newOID string
	ref    string
}

func gitReceivePack(c *reqCtx, repo *Repo) {
	w := c.w
	body := c.readBody()
	pr := &pktReader{data: body}
	var commands []refCommand
	caps := map[string]string{}
	first := true
	for {
		payload, kind := pr.next()
		if kind == "flush" {
			break
		}
		if kind != "line" {
			sendError(w, http.StatusBadRequest, "Malformed git request")
			return
		}
		line := string(payload)
		if first {
			first = false
			if i := strings.IndexByte(line, 0); i >= 0 {
				for _, kv := range strings.Fields(line[i+1:]) {
					k, v, _ := strings.Cut(kv, "=")
					caps[k] = v
				}
				line = line[:i]
			}
		}
		fields := strings.Fields(strings.TrimRight(line, "\n"))
		if len(fields) != 3 {
			sendError(w, http.StatusBadRequest, "Malformed git request")
			return
		}
		commands = append(commands, refCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}

	// The pack follows the flush, unless every command is a deletion.
	needPack := false
	for _, cmd := range commands {
		if cmd.newOID != zeroOID {
			needPack = true
		}
	}
	unpackOK := true
	if needPack {
		packData := pr.rest()
		if err := ingestPack(repo, packData); err != nil {
			unpackOK = false
		}
	}

	// Validate and apply per ref.
	type result struct {
		ref string
		ok  bool
		msg string
	}
	var results []result
	for _, cmd := range commands {
		if !unpackOK {
			results = append(results, result{cmd.ref, false, "unpack failed"})
			continue
		}
		current := repo.Refs[cmd.ref]
		if cmd.newOID == zeroOID {
			// Delete.
			if current == "" {
				results = append(results, result{cmd.ref, false, "reference does not exist"})
				continue
			}
			delete(repo.Refs, cmd.ref)
			results = append(results, result{cmd.ref, true, ""})
			continue
		}
		if repo.getObject(cmd.newOID) == nil {
			results = append(results, result{cmd.ref, false, "unknown object"})
			continue
		}
		if cmd.oldOID == zeroOID {
			// Create.
			if current != "" && current != cmd.newOID {
				results = append(results, result{cmd.ref, false, "reference already exists"})
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			results = append(results, result{cmd.ref, true, ""})
			continue
		}
		// Update: must be a fast-forward from the current value.
		if current != cmd.oldOID {
			results = append(results, result{cmd.ref, false, "stale ref"})
			continue
		}
		if !isAncestor(repo, cmd.oldOID, []string{cmd.newOID}) && cmd.oldOID != cmd.newOID {
			results = append(results, result{cmd.ref, false, "non-fast-forward"})
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		results = append(results, result{cmd.ref, true, ""})
	}

	if _, hasReport := caps["report-status"]; !hasReport {
		sendBytes(w, http.StatusOK, "application/x-git-receive-pack-result", pktFlush)
		return
	}
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktLine("unpack ok\n"))
	} else {
		report.Write(pktLine("unpack failed\n"))
	}
	for _, res := range results {
		if res.ok {
			report.Write(pktLine("ok " + res.ref + "\n"))
		} else {
			report.Write(pktLine("ng " + res.ref + " " + res.msg + "\n"))
		}
	}
	report.Write(pktFlush)

	var out bytes.Buffer
	if _, hasSideband := caps["side-band-64k"]; hasSideband {
		// Wrap each report pkt-line in band 1; the report's own flush
		// becomes a bare flush packet ending the stream.
		rep := &pktReader{data: report.Bytes()}
		for {
			start := rep.pos
			_, kind := rep.next()
			if kind == "eof" {
				break
			}
			raw := report.Bytes()[start:rep.pos]
			if kind == "flush" {
				out.Write(raw)
				break
			}
			wrapped := make([]byte, 0, len(raw)+1)
			wrapped = append(wrapped, 1)
			wrapped = append(wrapped, raw...)
			out.Write(pktLineBytes(wrapped))
		}
	} else {
		out.Write(report.Bytes())
	}
	sendBytes(w, http.StatusOK, "application/x-git-receive-pack-result", out.Bytes())
}

// ---- pack ingestion ----

type packObjectIn struct {
	typ  int
	data []byte
	// delta resolution
	isOFS    bool
	ofsBase  int // byte offset back from this object's start
	isREF    bool
	refBase  string
	rawStart int
}

// ingestPack parses a packfile and stores every object in the repo.
func ingestPack(repo *Repo, data []byte) error {
	if len(data) < 12+20 || string(data[:4]) != "PACK" {
		return fmt.Errorf("not a pack")
	}
	count := int(data[8])<<24 | int(data[9])<<16 | int(data[10])<<8 | int(data[11])
	pos := 12
	var objects []*packObjectIn
	for i := 0; i < count; i++ {
		start := pos
		if pos >= len(data) {
			return fmt.Errorf("truncated pack")
		}
		b := data[pos]
		pos++
		typ := int(b>>4) & 7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = data[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		obj := &packObjectIn{typ: typ, rawStart: start}
		switch typ {
		case 6: // OFS delta
			b = data[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				b = data[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			obj.isOFS = true
			obj.ofsBase = off
		case 7: // REF delta
			if pos+20 > len(data) {
				return fmt.Errorf("truncated ref delta")
			}
			obj.isREF = true
			obj.refBase = hex.EncodeToString(data[pos : pos+20])
			pos += 20
		}
		// Inflate the object stream; track bytes consumed via a reader.
		br := bytes.NewReader(data[pos:])
		zr, err := zlib.NewReader(br)
		if err != nil {
			return err
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			return err
		}
		_ = zr.Close()
		if len(raw) != size {
			return fmt.Errorf("object size mismatch")
		}
		consumed := len(data[pos:]) - br.Len()
		pos += consumed
		obj.data = raw
		objects = append(objects, obj)
	}

	// Resolve in order: non-delta objects first, then deltas whose bases
	// become available.
	resolved := map[int]string{} // pack index -> oid
	byOffset := map[int]int{}    // rawStart -> pack index
	for i, o := range objects {
		byOffset[o.rawStart] = i
	}
	pending := len(objects)
	for pending > 0 {
		progress := false
		for i, o := range objects {
			if _, done := resolved[i]; done {
				continue
			}
			switch {
			case o.typ >= 1 && o.typ <= 4:
				typName := []string{"", "commit", "tree", "blob", "tag"}[o.typ]
				resolved[i] = repo.putObject(typName, o.data)
				progress = true
			case o.isOFS:
				baseIdx, ok := byOffset[o.rawStart-o.ofsBase]
				if !ok {
					return fmt.Errorf("ofs base not in pack")
				}
				baseOID, done := resolved[baseIdx]
				if !done {
					continue
				}
				base := repo.getObject(baseOID)
				out, err := applyDelta(base.Data, o.data)
				if err != nil {
					return err
				}
				resolved[i] = repo.putObject(base.Type, out)
				progress = true
			case o.isREF:
				base := repo.getObject(o.refBase)
				if base == nil {
					continue // may be resolved later in this same pack
				}
				out, err := applyDelta(base.Data, o.data)
				if err != nil {
					return err
				}
				resolved[i] = repo.putObject(base.Type, out)
				progress = true
			}
		}
		if !progress {
			return fmt.Errorf("unresolvable pack objects")
		}
		pending = 0
		for i := range objects {
			if _, done := resolved[i]; !done {
				pending++
			}
		}
	}
	return nil
}

// applyDelta applies a git delta to base data.
func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, fmt.Errorf("delta truncated")
			}
			b := delta[pos]
			pos++
			v |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readVarint()
	if err != nil || srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	dstSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					cpOff |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					cpSize |= int(delta[pos]) << (8 * uint(i))
					pos++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out.Write(base[cpOff : cpOff+cpSize])
		} else if op != 0 {
			out.Write(delta[pos : pos+int(op)])
			pos += int(op)
		} else {
			return nil, fmt.Errorf("delta opcode 0")
		}
	}
	if out.Len() != dstSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out.Bytes(), nil
}
