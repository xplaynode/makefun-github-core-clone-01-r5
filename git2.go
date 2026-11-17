package main

// Upload-pack: protocol v2 ls-refs and fetch, protocol v0 fetch, and the
// pack builder shared by both.

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func gitUploadPack(c *reqCtx, repo *Repo) {
	w, r := c.w, c.r
	body := c.readBody()
	v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	pr := &pktReader{data: body}
	first, kind := pr.next()
	if kind != "line" {
		sendError(w, http.StatusBadRequest, "Malformed git request")
		return
	}
	if v2 {
		cmd := strings.TrimRight(string(first), "\n")
		switch {
		case cmd == "command=ls-refs":
			gitLsRefs(c, repo, pr)
		case cmd == "command=fetch":
			gitFetchV2(c, repo, pr)
		default:
			sendError(w, http.StatusBadRequest, "Unknown command")
		}
		return
	}
	gitFetchV0(c, repo, pr, first)
}

// ---- ls-refs ----

func gitLsRefs(c *reqCtx, repo *Repo, pr *pktReader) {
	// Capability lines until delim, then arguments until flush.
	for {
		_, kind := pr.next()
		if kind == "delim" || kind == "eof" {
			break
		}
	}
	peel, symrefs, unborn := false, false, false
	var prefixes []string
	for {
		payload, kind := pr.next()
		if kind != "line" {
			break
		}
		line := strings.TrimRight(string(payload), "\n")
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var out bytes.Buffer
	headOID := repo.Refs[repo.HeadRef]
	if matches("HEAD") {
		if headOID == "" {
			if unborn {
				line := "unborn HEAD"
				if symrefs {
					line += " symref-target:" + repo.HeadRef
				}
				out.Write(pktLine(line + "\n"))
			}
		} else {
			line := headOID + " HEAD"
			if symrefs {
				line += " symref-target:" + repo.HeadRef
			}
			out.Write(pktLine(line + "\n"))
		}
	}
	for _, ref := range sortedRefNames(repo) {
		if !matches(ref) {
			continue
		}
		oid := repo.Refs[ref]
		line := oid + " " + ref
		if peel {
			if commit := peelToCommit(repo, oid); commit != "" && commit != oid {
				line += " peeled:" + commit
			}
		}
		out.Write(pktLine(line + "\n"))
	}
	out.Write(pktFlush)
	sendBytes(c.w, http.StatusOK, "application/x-git-upload-pack-result", out.Bytes())
}

// ---- fetch request parsing ----

type fetchRequest struct {
	wants       []string
	haves       []string
	shallow     []string
	deepen      int
	deepenSince int64
	done        bool
}

// commitClosure returns commits reachable from tips in tip-to-root order
// (DFS pre-order), excluding anything in exclude. Excluded commits are
// still traversed through: a commit can be both wanted and had (a shallow
// boundary), and its parents remain reachable wants.
func commitClosure(repo *Repo, tips []string, exclude map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		commit := peelToCommit(repo, oid)
		if commit == "" || seen[commit] {
			return
		}
		seen[commit] = true
		if !exclude[commit] {
			out = append(out, commit)
		}
		c := parseCommit(repo.getObject(commit).Data)
		for _, p := range c.Parents {
			visit(p)
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return out
}

// commitClosureCut is commitClosure with a stop set: stop-set commits are
// included but their parents are not traversed. It models what a shallow
// client actually has: the boundary commits, but not their history.
func commitClosureCut(repo *Repo, tips []string, stopAt map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		commit := peelToCommit(repo, oid)
		if commit == "" || seen[commit] {
			return
		}
		seen[commit] = true
		out = append(out, commit)
		if stopAt[commit] {
			return
		}
		c := parseCommit(repo.getObject(commit).Data)
		for _, p := range c.Parents {
			visit(p)
		}
	}
	for _, t := range tips {
		visit(t)
	}
	return out
}

func ancestorSet(repo *Repo, tips []string) map[string]bool {
	set := map[string]bool{}
	for _, oid := range commitClosure(repo, tips, map[string]bool{}) {
		set[oid] = true
	}
	return set
}

// objectClosure returns every object reachable from the given commits:
// the commits, their trees (recursively) and blobs, and tag objects.
func objectClosure(repo *Repo, commits []string) map[string]bool {
	set := map[string]bool{}
	var walkTree func(oid string)
	walkTree = func(oid string) {
		if set[oid] {
			return
		}
		obj := repo.getObject(oid)
		if obj == nil || obj.Type != "tree" {
			return
		}
		set[oid] = true
		for _, e := range parseTree(obj.Data) {
			if e.Mode == "40000" {
				walkTree(e.OID)
			} else {
				set[e.OID] = true
			}
		}
	}
	for _, cm := range commits {
		set[cm] = true
		walkTree(parseCommit(repo.getObject(cm).Data).Tree)
	}
	return set
}

// buildPack assembles a packfile for wants minus haves. Object order:
// tag objects for tag wants first, then commits tip-to-root, then tree
// closures in the same commit order (the order other tools produce, which
// the pack trailer checksum pins down). Returns the pack and the shallow
// boundary (commits whose parents were cut by the depth limit).
func buildPack(repo *Repo, wants []string, haves []string, shallow []string, depth int) ([]byte, []string) {
	shallowSet := map[string]bool{}
	for _, s := range shallow {
		if cm := peelToCommit(repo, s); cm != "" {
			shallowSet[cm] = true
		}
	}
	haveList := commitClosureCut(repo, haves, shallowSet)
	haveCommits := map[string]bool{}
	for _, cm := range haveList {
		haveCommits[cm] = true
	}
	haveObjects := objectClosure(repo, haveList)
	commits := commitClosure(repo, wants, haveCommits)

	// Depth limiting: BFS levels from the wants (through commits the
	// client already has — a deepen request counts from the wants).
	var boundary []string
	if depth > 0 && len(commits) > 0 {
		level := map[string]int{}
		queue := []string{}
		for _, wnt := range wants {
			if cm := peelToCommit(repo, wnt); cm != "" && level[cm] == 0 {
				level[cm] = 1
				queue = append(queue, cm)
			}
		}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if level[cur] >= depth {
				continue
			}
			for _, p := range parseCommit(repo.getObject(cur).Data).Parents {
				if level[p] == 0 {
					level[p] = level[cur] + 1
					queue = append(queue, p)
				}
			}
		}
		var kept []string
		for _, cm := range commits {
			if level[cm] > 0 && level[cm] <= depth {
				kept = append(kept, cm)
			}
		}
		keptSet := map[string]bool{}
		for _, cm := range kept {
			keptSet[cm] = true
		}
		for _, cm := range kept {
			if level[cm] == depth {
				for _, p := range parseCommit(repo.getObject(cm).Data).Parents {
					if !keptSet[p] && !haveCommits[p] {
						boundary = append(boundary, cm)
						break
					}
				}
			}
		}
		commits = kept
	}

	emitted := map[string]bool{}
	var objects []string
	emit := func(oid string) {
		if oid == "" || emitted[oid] || repo.getObject(oid) == nil {
			return
		}
		if haveObjects[oid] {
			return
		}
		emitted[oid] = true
		objects = append(objects, oid)
	}
	// Tag objects for tag wants.
	for _, wnt := range wants {
		if obj := repo.getObject(wnt); obj != nil && obj.Type == "tag" {
			emit(wnt)
		}
	}
	for _, cm := range commits {
		emit(cm)
	}
	// Tree closures, in commit order (tip first).
	for i := 0; i < len(commits); i++ {
		treeOID := parseCommit(repo.getObject(commits[i]).Data).Tree
		var walkTree func(oid string)
		walkTree = func(oid string) {
			if emitted[oid] {
				return
			}
			obj := repo.getObject(oid)
			if obj == nil || obj.Type != "tree" {
				return
			}
			emit(oid)
			for _, e := range parseTree(obj.Data) {
				if e.Mode == "40000" {
					walkTree(e.OID)
				} else {
					emit(e.OID)
				}
			}
		}
		walkTree(treeOID)
	}

	var pack bytes.Buffer
	pack.WriteString("PACK")
	pack.Write([]byte{0, 0, 0, 2})
	var cnt [4]byte
	cnt[0] = byte(len(objects) >> 24)
	cnt[1] = byte(len(objects) >> 16)
	cnt[2] = byte(len(objects) >> 8)
	cnt[3] = byte(len(objects))
	pack.Write(cnt[:])
	for _, oid := range objects {
		obj := repo.getObject(oid)
		pack.Write(packObjectHeader(obj.Type, len(obj.Data)))
		pack.Write(repo.zstream(oid))
	}
	sum := sha1.Sum(pack.Bytes())
	pack.Write(sum[:])
	return pack.Bytes(), boundary
}

func packObjectHeader(typ string, size int) []byte {
	code := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}[typ]
	first := byte(code<<4) | byte(size&0x0f)
	size >>= 4
	var out []byte
	if size > 0 {
		first |= 0x80
	}
	out = append(out, first)
	for size > 0 {
		b := byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// sidebandPacket wraps data in band-1 packets of at most 65515 data bytes.
func sidebandPackets(data []byte) []byte {
	var out bytes.Buffer
	for len(data) > 0 {
		n := len(data)
		if n > 65515 {
			n = 65515
		}
		chunk := make([]byte, 0, n+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, data[:n]...)
		out.Write(pktLineBytes(chunk))
		data = data[n:]
	}
	return out.Bytes()
}

// ---- fetch v2 ----

func gitFetchV2(c *reqCtx, repo *Repo, pr *pktReader) {
	// Capability lines until delim.
	for {
		_, kind := pr.next()
		if kind == "delim" || kind == "eof" {
			break
		}
	}
	var freq fetchRequest
	for {
		payload, kind := pr.next()
		if kind != "line" {
			break
		}
		line := strings.TrimRight(string(payload), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			freq.wants = append(freq.wants, strings.TrimPrefix(line, "want "))
		case strings.HasPrefix(line, "have "):
			freq.haves = append(freq.haves, strings.TrimPrefix(line, "have "))
		case strings.HasPrefix(line, "shallow "):
			freq.shallow = append(freq.shallow, strings.TrimPrefix(line, "shallow "))
		case strings.HasPrefix(line, "deepen "):
			freq.deepen, _ = strconv.Atoi(strings.TrimPrefix(line, "deepen "))
		case strings.HasPrefix(line, "deepen-since "):
			freq.deepenSince, _ = strconv.ParseInt(strings.TrimPrefix(line, "deepen-since "), 10, 64)
		case line == "done":
			freq.done = true
		}
	}

	pack, boundary := buildPack(repo, freq.wants, freq.haves, freq.shallow, freq.deepen)

	var out bytes.Buffer
	// acknowledgments
	out.Write(pktLine("acknowledgments\n"))
	common := ""
	for _, h := range freq.haves {
		if peelToCommit(repo, h) != "" {
			// A have the client already has is common ground when it is an
			// ancestor of (or equal to) a want; with no wants, ACK the have.
			if len(freq.wants) == 0 || isAncestor(repo, h, freq.wants) {
				common = h
				break
			}
		}
	}
	if common != "" {
		out.Write(pktLine("ACK " + common + "\n"))
	} else {
		out.Write(pktLine("NAK\n"))
	}
	out.Write(pktFlush)
	// shallow-info
	if len(boundary) > 0 || len(freq.shallow) > 0 {
		out.Write(pktLine("shallow-info\n"))
		for _, b := range boundary {
			out.Write(pktLine("shallow " + b + "\n"))
		}
		out.Write(pktFlush)
	}
	// packfile
	out.Write(pktLine("packfile\n"))
	out.Write(sidebandPackets(pack))
	out.Write(pktFlush)
	sendBytes(c.w, http.StatusOK, "application/x-git-upload-pack-result", out.Bytes())
}

// isAncestor reports whether oid is an ancestor of (or equal to) any tip.
// The search stops as soon as the target is found, which makes the common
// fast-forward check (the old tip is the new tip's parent) O(1).
func isAncestor(repo *Repo, oid string, tips []string) bool {
	target := peelToCommit(repo, oid)
	if target == "" {
		return false
	}
	seen := map[string]bool{}
	var visit func(cur string) bool
	visit = func(cur string) bool {
		commit := peelToCommit(repo, cur)
		if commit == "" || seen[commit] {
			return false
		}
		if commit == target {
			return true
		}
		seen[commit] = true
		c := parseCommit(repo.getObject(commit).Data)
		for _, p := range c.Parents {
			if visit(p) {
				return true
			}
		}
		return false
	}
	for _, t := range tips {
		if visit(t) {
			return true
		}
	}
	return false
}

// ---- fetch v0 ----

func gitFetchV0(c *reqCtx, repo *Repo, pr *pktReader, first []byte) {
	var freq fetchRequest
	// First line: "want <oid> <capabilities>".
	line := strings.TrimRight(string(first), "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[0] == "want" {
		freq.wants = append(freq.wants, fields[1])
	} else {
		sendError(c.w, http.StatusBadRequest, "Malformed git request")
		return
	}
	// The first section also carries shallow and deepen lines in v0.
	for {
		payload, kind := pr.next()
		if kind == "flush" {
			break
		}
		if kind != "line" {
			break
		}
		l := strings.TrimRight(string(payload), "\n")
		switch {
		case strings.HasPrefix(l, "want "):
			freq.wants = append(freq.wants, strings.TrimPrefix(l, "want "))
		case strings.HasPrefix(l, "shallow "):
			freq.shallow = append(freq.shallow, strings.TrimPrefix(l, "shallow "))
		case strings.HasPrefix(l, "deepen "):
			freq.deepen, _ = strconv.Atoi(strings.TrimPrefix(l, "deepen "))
		case strings.HasPrefix(l, "deepen-since "):
			freq.deepenSince, _ = strconv.ParseInt(strings.TrimPrefix(l, "deepen-since "), 10, 64)
		}
	}
	// have lines until "done".
	for {
		payload, kind := pr.next()
		if kind != "line" {
			break
		}
		l := strings.TrimRight(string(payload), "\n")
		switch {
		case strings.HasPrefix(l, "have "):
			freq.haves = append(freq.haves, strings.TrimPrefix(l, "have "))
		case strings.HasPrefix(l, "shallow "):
			freq.shallow = append(freq.shallow, strings.TrimPrefix(l, "shallow "))
		case strings.HasPrefix(l, "deepen "):
			freq.deepen, _ = strconv.Atoi(strings.TrimPrefix(l, "deepen "))
		case l == "done":
			freq.done = true
		}
	}

	pack, boundary := buildPack(repo, freq.wants, freq.haves, freq.shallow, freq.deepen)
	var out bytes.Buffer
	for _, b := range boundary {
		out.Write(pktLine("shallow " + b + "\n"))
	}
	common := ""
	for _, h := range freq.haves {
		if len(freq.wants) == 0 || isAncestor(repo, h, freq.wants) {
			common = h
			break
		}
	}
	if common != "" {
		out.Write(pktLine(fmt.Sprintf("ACK %s\n", common)))
	} else {
		out.Write(pktLine("NAK\n"))
	}
	out.Write(sidebandPackets(pack))
	out.Write(pktFlush)
	sendBytes(c.w, http.StatusOK, "application/x-git-upload-pack-result", out.Bytes())
}
