package main

// Contents, labels, issues and pull requests for the REST surface, plus the
// tree-diff and merge machinery the pull surface shares with GraphQL.

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---- tree flattening / rebuilding ----

type flatFile struct {
	Mode string
	OID  string
}

// flattenTree expands a tree object into path -> file entries (recursive).
func flattenTree(repo *Repo, treeOID string) map[string]flatFile {
	out := map[string]flatFile{}
	var walk func(prefix, oid string)
	walk = func(prefix, oid string) {
		obj := repo.getObject(oid)
		if obj == nil || obj.Type != "tree" {
			return
		}
		for _, e := range parseTree(obj.Data) {
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(p, e.OID)
			} else {
				out[p] = flatFile{Mode: e.Mode, OID: e.OID}
			}
		}
	}
	if treeOID != "" {
		walk("", treeOID)
	}
	return out
}

// treeFromFiles builds nested trees for a flat path map and stores them.
func treeFromFiles(repo *Repo, files map[string]flatFile) string {
	if len(files) == 0 {
		return repo.putObject("tree", nil)
	}
	// Group by first path segment.
	rootEntries := []TreeEntry{}
	subdirs := map[string]map[string]flatFile{}
	for path, f := range files {
		head, tail, found := strings.Cut(path, "/")
		if !found {
			rootEntries = append(rootEntries, TreeEntry{Mode: f.Mode, Name: head, OID: f.OID})
		} else {
			if subdirs[head] == nil {
				subdirs[head] = map[string]flatFile{}
			}
			subdirs[head][tail] = f
		}
	}
	for name, sub := range subdirs {
		oid := treeFromFiles(repo, sub)
		rootEntries = append(rootEntries, TreeEntry{Mode: "40000", Name: name, OID: oid})
	}
	sortTreeEntries(rootEntries)
	return repo.putObject("tree", buildTree(rootEntries))
}

func treeAtRef(repo *Repo, ref string) (treeOID string, tip string) {
	if ref == "" {
		tip = repo.Refs[repo.HeadRef]
	} else if oid, ok := repo.Refs["refs/heads/"+ref]; ok {
		tip = oid
	} else if oid, ok := repo.Refs[ref]; ok {
		tip = oid
	} else {
		tip = ref // an oid
	}
	obj := repo.getObject(tip)
	if obj == nil || obj.Type != "commit" {
		return "", tip
	}
	return parseCommit(obj.Data).Tree, tip
}

// commitFiles writes a commit with the given flat file map on a branch.
func commitFiles(repo *Repo, branchRef, message string, files map[string]flatFile) string {
	treeOID := treeFromFiles(repo, files)
	_, tip := treeAtRef(repo, branchRef)
	c := &Commit{
		Tree:      treeOID,
		Author:    identLine(viewerUser, time.Now()),
		Committer: identLine(viewerUser, time.Now()),
		Message:   message,
	}
	if tip != "" {
		c.Parents = []string{tip}
	}
	oid := repo.putObject("commit", buildCommit(c))
	repo.Refs[branchRef] = oid
	return oid
}

// ---- contents ----

func restContents(c *reqCtx, repo *Repo, parts []string) {
	w, r := c.w, c.r
	path := strings.Join(parts, "/")
	switch r.Method {
	case http.MethodGet:
		ref := r.URL.Query().Get("ref")
		treeOID, _ := treeAtRef(repo, ref)
		if path == "" {
			sendJSON(w, http.StatusOK, dirListingJSON(repo, treeOID, ""))
			return
		}
		// Walk to the target.
		segments := strings.Split(path, "/")
		curTree := treeOID
		for i, segName := range segments {
			obj := repo.getObject(curTree)
			if obj == nil || obj.Type != "tree" {
				sendError(w, http.StatusNotFound, "Not Found")
				return
			}
			var found *TreeEntry
			for _, e := range parseTree(obj.Data) {
				if e.Name == segName {
					ee := e
					found = &ee
					break
				}
			}
			if found == nil {
				sendError(w, http.StatusNotFound, "Not Found")
				return
			}
			if i == len(segments)-1 {
				if found.Mode == "40000" {
					sendJSON(w, http.StatusOK, dirListingJSON(repo, found.OID, path))
					return
				}
				blob := repo.getObject(found.OID)
				w2 := &jw{}
				w2.raw(`{"type":"file","name":`).str(found.Name)
				w2.raw(`,"path":`).str(path)
				w2.raw(`,"sha":`).str(found.OID)
				w2.raw(`,"size":`).num(len(blob.Data))
				w2.raw(`,"encoding":"base64","content":`).str(base64.StdEncoding.EncodeToString(blob.Data))
				w2.raw("}\n")
				sendJSON(w, http.StatusOK, w2.bytes())
				return
			}
			if found.Mode != "40000" {
				sendError(w, http.StatusNotFound, "Not Found")
				return
			}
			curTree = found.OID
		}
	case http.MethodPut:
		body, ok := c.jsonBody()
		if !ok {
			sendError(w, http.StatusBadRequest, "Malformed JSON")
			return
		}
		contentB64, _ := strField(body, "content")
		raw, err := base64.StdEncoding.DecodeString(contentB64)
		if err != nil {
			sendError(w, http.StatusUnprocessableEntity, "Validation Failed: content is not valid base64")
			return
		}
		message, _ := strField(body, "message")
		branch, _ := strField(body, "branch")
		branchRef := repo.HeadRef
		if branch != "" {
			branchRef = "refs/heads/" + branch
		}
		treeOID, _ := treeAtRef(repo, strings.TrimPrefix(branchRef, "refs/heads/"))
		files := flattenTree(repo, treeOID)
		existing, exists := files[path]
		shaGiven, hasSHA := strField(body, "sha")
		if exists && hasSHA && shaGiven != existing.OID {
			sendError(w, http.StatusConflict, "Conflict: the file has changed")
			return
		}
		blobOID := repo.putObject("blob", raw)
		files[path] = flatFile{Mode: "100644", OID: blobOID}
		commitOID := commitFiles(repo, branchRef, message, files)
		status := http.StatusOK
		if !exists {
			status = http.StatusCreated
		}
		w2 := &jw{}
		w2.raw(`{"content":{"sha":`).str(blobOID).raw(`,"encoding":"base64"},"commit":{"sha":`).str(commitOID).raw(`}}` + "\n")
		sendJSON(w, status, w2.bytes())
	case http.MethodDelete:
		body, ok := c.jsonBody()
		if !ok {
			sendError(w, http.StatusBadRequest, "Malformed JSON")
			return
		}
		message, _ := strField(body, "message")
		branch, _ := strField(body, "branch")
		branchRef := repo.HeadRef
		if branch != "" {
			branchRef = "refs/heads/" + branch
		}
		treeOID, _ := treeAtRef(repo, strings.TrimPrefix(branchRef, "refs/heads/"))
		files := flattenTree(repo, treeOID)
		existing, exists := files[path]
		if !exists {
			sendError(w, http.StatusNotFound, "Not Found")
			return
		}
		if shaGiven, hasSHA := strField(body, "sha"); hasSHA && shaGiven != existing.OID {
			sendError(w, http.StatusConflict, "Conflict: the file has changed")
			return
		}
		delete(files, path)
		commitOID := commitFiles(repo, branchRef, message, files)
		w2 := &jw{}
		w2.raw(`{"content":null,"commit":{"sha":`).str(commitOID).raw(`}}` + "\n")
		sendJSON(w, http.StatusOK, w2.bytes())
	default:
		sendError(w, http.StatusNotFound, "Not Found")
	}
}

func dirListingJSON(repo *Repo, treeOID, prefix string) []byte {
	w := &jw{}
	w.raw("[")
	obj := repo.getObject(treeOID)
	first := true
	if obj != nil && obj.Type == "tree" {
		for _, e := range parseTree(obj.Data) {
			if !first {
				w.raw(",")
			}
			first = false
			typ := "file"
			if e.Mode == "40000" {
				typ = "dir"
			}
			p := e.Name
			if prefix != "" {
				p = prefix + "/" + e.Name
			}
			w.raw(`{"type":`).str(typ).raw(`,"name":`).str(e.Name).raw(`,"path":`).str(p).raw(`,"sha":`).str(e.OID).raw(`}`)
		}
	}
	w.raw("]\n")
	return w.bytes()
}

// ---- labels ----

func restLabels(c *reqCtx, repo *Repo, parts []string) {
	w, r := c.w, c.r
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			name, _ := strField(body, "name")
			if name == "" {
				sendError(w, http.StatusUnprocessableEntity, "Validation Failed: name is required")
				return
			}
			repo.addLabel(name)
			w2 := &jw{}
			w2.raw(`{"name":`).str(name).raw(`,"color":"","description":null}` + "\n")
			sendJSON(w, http.StatusCreated, w2.bytes())
		case http.MethodGet:
			w2 := &jw{}
			w2.raw("[")
			for i, l := range repo.Labels {
				if i > 0 {
					w2.raw(",")
				}
				w2.raw(`{"name":`).str(l.Name).raw(`}`)
			}
			w2.raw("]\n")
			sendJSON(w, http.StatusOK, w2.bytes())
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	sendError(w, http.StatusNotFound, "Not Found")
}

// ---- issues ----

func labelNames(body map[string]any) []string {
	v, ok := body["labels"]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]any:
			if n, ok := t["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}

func restIssues(c *reqCtx, repo *Repo, parts []string) {
	w, r := c.w, c.r
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			title, _ := strField(body, "title")
			bodyText, _ := strField(body, "body")
			labels := labelNames(body)
			for _, name := range labels {
				if repo.label(name) == nil {
					sendError(w, http.StatusUnprocessableEntity, "Validation Failed: label '"+name+"' does not exist")
					return
				}
			}
			is := store.createIssue(repo, title, bodyText, labels)
			sendJSON(w, http.StatusCreated, issueJSON(is))
		case http.MethodGet:
			q := r.URL.Query()
			stateFilter := q.Get("state")
			if stateFilter == "" {
				stateFilter = "open"
			}
			direction := q.Get("direction")
			var list []*Issue
			for _, is := range repo.Issues {
				if stateFilter == "all" || is.State == stateFilter {
					list = append(list, is)
				}
			}
			sort.Slice(list, func(i, j int) bool {
				if direction == "desc" {
					return list[i].Number > list[j].Number
				}
				return list[i].Number < list[j].Number
			})
			w2 := &jw{}
			w2.raw("[")
			for i, is := range list {
				if i > 0 {
					w2.raw(",")
				}
				w2.raw(strings.TrimRight(string(issueJSON(is)), "\n"))
			}
			w2.raw("]\n")
			sendJSON(w, http.StatusOK, w2.bytes())
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	is := repo.Issues[num]
	if is == nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodGet:
			sendJSON(w, http.StatusOK, issueJSON(is))
		case http.MethodPatch:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			if v, ok := strField(body, "state"); ok {
				is.State = v
			}
			if v, ok := strField(body, "title"); ok {
				is.Title = v
			}
			if v, ok := strField(body, "body"); ok {
				is.Body = v
			}
			if _, present := body["labels"]; present {
				labels := labelNames(body)
				for _, name := range labels {
					if repo.label(name) == nil {
						sendError(w, http.StatusUnprocessableEntity, "Validation Failed: label '"+name+"' does not exist")
						return
					}
				}
				is.Labels = labels
			}
			is.jsonCache = nil
			sendJSON(w, http.StatusOK, issueJSON(is))
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(parts) == 2 && parts[1] == "comments" {
		switch r.Method {
		case http.MethodPost:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			text, _ := strField(body, "body")
			cm := store.createComment(is, text)
			is.jsonCache = nil
			sendJSON(w, http.StatusCreated, commentJSON(cm))
		case http.MethodGet:
			w2 := &jw{}
			w2.raw("[")
			for i, cm := range is.Comments {
				if i > 0 {
					w2.raw(",")
				}
				w2.raw(strings.TrimRight(string(commentJSON(cm)), "\n"))
			}
			w2.raw("]\n")
			sendJSON(w, http.StatusOK, w2.bytes())
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	sendError(w, http.StatusNotFound, "Not Found")
}

// ---- pulls ----

func restPulls(c *reqCtx, repo *Repo, parts []string) {
	w, r := c.w, c.r
	if len(parts) == 0 {
		switch r.Method {
		case http.MethodPost:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			title, _ := strField(body, "title")
			bodyText, _ := strField(body, "body")
			head, _ := strField(body, "head")
			base, _ := strField(body, "base")
			head = stripOwnerPrefix(head)
			base = stripOwnerPrefix(base)
			if repo.Refs["refs/heads/"+head] == "" || repo.Refs["refs/heads/"+base] == "" {
				sendError(w, http.StatusUnprocessableEntity, "Validation Failed: head or base branch does not exist")
				return
			}
			p := store.createPull(repo, title, bodyText, head, base)
			sendJSON(w, http.StatusCreated, pullJSON(repo, p))
		case http.MethodGet:
			stateFilter := r.URL.Query().Get("state")
			if stateFilter == "" {
				stateFilter = "open"
			}
			var list []*Pull
			for _, p := range repo.Pulls {
				if stateFilter == "all" || p.State == stateFilter {
					list = append(list, p)
				}
			}
			sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
			w2 := &jw{}
			w2.raw("[")
			for i, p := range list {
				if i > 0 {
					w2.raw(",")
				}
				w2.raw(strings.TrimRight(string(pullJSON(repo, p)), "\n"))
			}
			w2.raw("]\n")
			sendJSON(w, http.StatusOK, w2.bytes())
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(parts[0])
	if err != nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	p := repo.Pulls[num]
	if p == nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		sendJSON(w, http.StatusOK, pullJSON(repo, p))
		return
	}
	if len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodGet {
		sendJSON(w, http.StatusOK, pullFilesJSON(repo, p))
		return
	}
	if len(parts) == 2 && parts[1] == "merge" && (r.Method == http.MethodPut || r.Method == http.MethodPost) {
		body, _ := c.jsonBody()
		method, _ := strField(body, "merge_method")
		sha, conflict := mergePull(repo, p, method)
		if conflict {
			sendError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
			return
		}
		if sha == "" {
			sendError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
			return
		}
		w2 := &jw{}
		w2.raw(`{"sha":`).str(sha).raw(`,"merged":true,"message":"Pull Request successfully merged"}` + "\n")
		sendJSON(w, http.StatusOK, w2.bytes())
		return
	}
	sendError(w, http.StatusNotFound, "Not Found")
}

func stripOwnerPrefix(ref string) string {
	if i := strings.Index(ref, ":"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// ---- diff & merge ----

type fileChange struct {
	Path      string
	Status    string // added | removed | modified
	Additions int
	Deletions int
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines between two texts via LCS.
func lineDiff(a, b string) (additions, deletions int) {
	la := strings.Split(a, "\n")
	lb := strings.Split(b, "\n")
	// LCS length with two rows.
	prev := make([]int, len(lb)+1)
	cur := make([]int, len(lb)+1)
	for i := 1; i <= len(la); i++ {
		for j := 1; j <= len(lb); j++ {
			if la[i-1] == lb[j-1] {
				cur[j] = prev[j-1] + 1
			} else if prev[j] >= cur[j-1] {
				cur[j] = prev[j]
			} else {
				cur[j] = cur[j-1]
			}
		}
		prev, cur = cur, prev
	}
	common := prev[len(lb)]
	return len(lb) - common, len(la) - common
}

func diffTrees(repo *Repo, baseTree, headTree string) []fileChange {
	baseFiles := flattenTree(repo, baseTree)
	headFiles := flattenTree(repo, headTree)
	paths := map[string]bool{}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range headFiles {
		paths[p] = true
	}
	var sorted []string
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	var out []fileChange
	for _, p := range sorted {
		bf, inBase := baseFiles[p]
		hf, inHead := headFiles[p]
		switch {
		case inBase && !inHead:
			out = append(out, fileChange{Path: p, Status: "removed", Deletions: lineCount(repo.getObject(bf.OID).Data)})
		case !inBase && inHead:
			out = append(out, fileChange{Path: p, Status: "added", Additions: lineCount(repo.getObject(hf.OID).Data)})
		case bf.OID != hf.OID:
			add, del := lineDiff(string(repo.getObject(bf.OID).Data), string(repo.getObject(hf.OID).Data))
			out = append(out, fileChange{Path: p, Status: "modified", Additions: add, Deletions: del})
		}
	}
	return out
}

func pullFilesJSON(repo *Repo, p *Pull) []byte {
	baseTree, _ := treeAtRef(repo, p.BaseRef)
	headTree, _ := treeAtRef(repo, p.HeadRef)
	changes := diffTrees(repo, baseTree, headTree)
	w := &jw{}
	w.raw("[")
	for i, ch := range changes {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"filename":`).str(ch.Path)
		w.raw(`,"status":`).str(ch.Status)
		w.raw(`,"additions":`).num(ch.Additions)
		w.raw(`,"deletions":`).num(ch.Deletions)
		w.raw(`,"changes":`).num(ch.Additions + ch.Deletions)
		w.raw(`}`)
	}
	w.raw("]\n")
	return w.bytes()
}

// ancestors returns the set of commits reachable from tip (including tip).
func ancestors(repo *Repo, tip string) map[string]bool {
	set := map[string]bool{}
	for _, oid := range walkCommits(repo, tip) {
		set[oid] = true
	}
	return set
}

func mergeBase(repo *Repo, a, b string) string {
	anc := ancestors(repo, a)
	for _, oid := range walkCommits(repo, b) {
		if anc[oid] {
			return oid
		}
	}
	return ""
}

// mergePull merges p's head into its base. Returns the resulting commit sha,
// or conflict=true when both sides changed the same file differently.
func mergePull(repo *Repo, p *Pull, method string) (string, bool) {
	if p.Merged {
		return p.MergeCommitSHA, false
	}
	baseRef := "refs/heads/" + p.BaseRef
	headRef := "refs/heads/" + p.HeadRef
	baseTip := repo.Refs[baseRef]
	headTip := repo.Refs[headRef]
	if baseTip == "" || headTip == "" {
		return "", true
	}
	mb := mergeBase(repo, baseTip, headTip)
	mbTree, _ := treeAtRef(repo, mb)
	baseTree, _ := treeAtRef(repo, p.BaseRef)
	headTree, _ := treeAtRef(repo, p.HeadRef)
	mbFiles := flattenTree(repo, mbTree)
	baseFiles := flattenTree(repo, baseTree)
	headFiles := flattenTree(repo, headTree)

	changed := func(files map[string]flatFile, path string) (flatFile, bool, bool) {
		f, ok := files[path]
		mf, mok := mbFiles[path]
		if ok != mok {
			return f, ok, true
		}
		if ok && f.OID != mf.OID {
			return f, ok, true
		}
		return f, ok, false
	}
	paths := map[string]bool{}
	for pth := range baseFiles {
		paths[pth] = true
	}
	for pth := range headFiles {
		paths[pth] = true
	}
	merged := map[string]flatFile{}
	for pth, f := range baseFiles {
		merged[pth] = f
	}
	for pth := range paths {
		_, _, baseChangedPath := changed(baseFiles, pth)
		headF, headOK, headChangedPath := changed(headFiles, pth)
		if !headChangedPath {
			continue
		}
		if baseChangedPath {
			baseF, baseOK := baseFiles[pth]
			if baseOK != headOK || (baseOK && headOK && baseF.OID != headF.OID) {
				return "", true
			}
			continue
		}
		if headOK {
			merged[pth] = headF
		} else {
			delete(merged, pth)
		}
	}
	treeOID := treeFromFiles(repo, merged)
	now := time.Now()
	c := &Commit{
		Tree:      treeOID,
		Author:    identLine(viewerUser, now),
		Committer: identLine(viewerUser, now),
	}
	if method == "merge" {
		c.Parents = []string{baseTip, headTip}
		c.Message = fmt.Sprintf("Merge pull request #%d from %s\n", p.Number, p.HeadRef)
	} else {
		c.Parents = []string{baseTip}
		c.Message = fmt.Sprintf("%s (#%d)\n", p.Title, p.Number)
	}
	sha := repo.putObject("commit", buildCommit(c))
	repo.Refs[baseRef] = sha
	p.Merged = true
	p.State = "closed"
	p.MergedAt = isoTime(now)
	p.MergeCommitSHA = sha
	return sha, false
}
