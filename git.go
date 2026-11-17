package main

// The git smart-HTTP protocol: info/refs advertisements, protocol v2
// ls-refs and fetch, protocol v0 fetch, and v0 receive-pack with
// report-status. Packs are built with the byte-exact compressor so pack
// bytes (and therefore their SHA-1 trailers) match what other tools
// produce for the same object sets.

import (
	"bytes"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

const zeroOID = "0000000000000000000000000000000000000000"
const agentString = "lucky-forge/1.0.0"

// ---- pkt-line ----

func pktLine(payload string) []byte {
	n := len(payload) + 4
	return []byte(fmt.Sprintf("%04x%s", n, payload))
}

func pktLineBytes(payload []byte) []byte {
	n := len(payload) + 4
	head := []byte(fmt.Sprintf("%04x", n))
	return append(head, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

type pktReader struct {
	data []byte
	pos  int
	err  bool
}

// next returns the next pkt-line payload. kind: "line", "flush", "delim",
// "end" (response-end), or "eof".
func (p *pktReader) next() (payload []byte, kind string) {
	if p.pos+4 > len(p.data) {
		return nil, "eof"
	}
	n, err := strconv.ParseUint(string(p.data[p.pos:p.pos+4]), 16, 32)
	if err != nil {
		p.err = true
		return nil, "eof"
	}
	switch n {
	case 0:
		p.pos += 4
		return nil, "flush"
	case 1:
		p.pos += 4
		return nil, "delim"
	case 2:
		p.pos += 4
		return nil, "end"
	}
	if n < 4 || p.pos+int(n) > len(p.data) {
		p.err = true
		return nil, "eof"
	}
	payload = p.data[p.pos+4 : p.pos+int(n)]
	p.pos += int(n)
	return payload, "line"
}

func (p *pktReader) rest() []byte {
	if p.pos >= len(p.data) {
		return nil
	}
	return p.data[p.pos:]
}

// ---- repo path parsing ----

func parseGitPath(path string) (owner, repo, action string, ok bool) {
	// /{owner}/{repo}.git/{action}
	trimmed := strings.TrimPrefix(path, "/")
	parts := strings.SplitN(trimmed, "/", 3)
	if len(parts) != 3 || !strings.HasSuffix(parts[1], ".git") {
		return "", "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), parts[2], true
}

func handleGit(c *reqCtx) {
	w, r := c.w, c.r
	owner, name, action, ok := parseGitPath(r.URL.Path)
	if !ok {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !authorised(r) {
		sendError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	// Reads (advertisements, ls-refs, fetch) run concurrently under the
	// read lock; only receive-pack mutates.
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "git-receive-pack") {
		store.mu.Lock()
		defer store.mu.Unlock()
	} else {
		store.mu.RLock()
		defer store.mu.RUnlock()
	}
	repo := store.repo(owner, name)
	if repo == nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	switch {
	case r.Method == http.MethodGet && action == "info/refs":
		gitAdvertise(c, repo)
	case r.Method == http.MethodPost && action == "git-upload-pack":
		gitUploadPack(c, repo)
	case r.Method == http.MethodPost && action == "git-receive-pack":
		gitReceivePack(c, repo)
	default:
		sendError(w, http.StatusNotFound, "Not Found")
	}
}

// sortedRefs returns ref names in advertisement order: HEAD first, then
// lexicographic.
func sortedRefNames(repo *Repo) []string {
	var names []string
	for ref := range repo.Refs {
		names = append(names, ref)
	}
	sort.Strings(names)
	return names
}

// peelToCommit resolves an object to the commit it ultimately points at
// (through annotated tags). Returns "" when it is not commit-like.
func peelToCommit(repo *Repo, oid string) string {
	for {
		obj := repo.getObject(oid)
		if obj == nil {
			return ""
		}
		if obj.Type == "commit" {
			return oid
		}
		if obj.Type == "tag" {
			for _, line := range strings.Split(string(obj.Data), "\n") {
				if strings.HasPrefix(line, "object ") {
					oid = strings.TrimSpace(line[len("object "):])
					break
				}
				if line == "" {
					return ""
				}
			}
			continue
		}
		return ""
	}
}

func gitAdvertise(c *reqCtx, repo *Repo) {
	w, r := c.w, c.r
	service := r.URL.Query().Get("service")
	v2 := strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	switch service {
	case "git-upload-pack":
		if v2 {
			var out bytes.Buffer
			out.Write(pktLine("version 2\n"))
			out.Write(pktLine("agent=" + agentString + "\n"))
			out.Write(pktLine("ls-refs=unborn\n"))
			out.Write(pktLine("fetch=shallow wait-for-done\n"))
			out.Write(pktLine("object-format=sha1\n"))
			out.Write(pktFlush)
			sendBytes(w, http.StatusOK, "application/x-git-upload-pack-advertisement", out.Bytes())
			return
		}
		caps := "side-band-64k ofs-delta shallow deepen-since deepen-not multi_ack_detailed no-done agent=" + agentString
		sendBytes(w, http.StatusOK, "application/x-git-upload-pack-advertisement", advertiseV0(repo, "# service=git-upload-pack\n", caps))
	case "git-receive-pack":
		caps := "report-status delete-refs side-band-64k ofs-delta agent=" + agentString
		sendBytes(w, http.StatusOK, "application/x-git-receive-pack-advertisement", advertiseV0(repo, "# service=git-receive-pack\n", caps))
	default:
		sendError(w, http.StatusBadRequest, "Unknown service")
	}
}

// advertiseV0 renders a v0 ref advertisement: service line, flush, then one
// line per ref (HEAD first); the first line carries the capabilities.
func advertiseV0(repo *Repo, serviceLine, caps string) []byte {
	var out bytes.Buffer
	out.Write(pktLine(serviceLine))
	out.Write(pktFlush)
	refs := sortedRefNames(repo)
	type line struct{ oid, name string }
	var lines []line
	headOID := repo.Refs[repo.HeadRef]
	if headOID == "" {
		headOID = zeroOID
	}
	lines = append(lines, line{headOID, "HEAD"})
	for _, ref := range refs {
		lines = append(lines, line{repo.Refs[ref], ref})
		if commit := peelToCommit(repo, repo.Refs[ref]); commit != "" && commit != repo.Refs[ref] {
			lines = append(lines, line{commit, ref + "^{}"})
		}
	}
	if len(repo.Refs) == 0 {
		// Empty repository: a single capabilities^{} pseudo-ref.
		payload := zeroOID + " capabilities^{}\x00" + caps + "\n"
		out.Write(pktLine(payload))
		return out.Bytes()
	}
	for i, l := range lines {
		payload := l.oid + " " + l.name
		if i == 0 {
			payload += "\x00" + caps
		}
		payload += "\n"
		out.Write(pktLine(payload))
	}
	return out.Bytes()
}
