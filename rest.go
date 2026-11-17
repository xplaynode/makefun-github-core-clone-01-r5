package main

// The REST v3 surface. Every reply is JSON (including errors), every reply
// carries the request id header set by the router, and repository access is
// scoped to the owner in the path: a repository that exists under another
// owner is a 404, never a 403.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func isoTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// ---- serialisers ----

func repoJSON(r *Repo) []byte {
	if r.jsonCache != nil {
		return r.jsonCache
	}
	w := &jw{}
	w.raw(`{"id":`).num(r.ID)
	w.raw(`,"name":`).str(r.Name)
	w.raw(`,"full_name":`).str(r.Owner + "/" + r.Name)
	w.raw(`,"private":`).boolean(r.Private)
	w.raw(`,"owner":{"login":`).str(r.Owner).raw(`,"id":`).num(userID(r.Owner)).raw(`}`)
	w.raw(`,"description":`).str(r.Description)
	w.raw(`,"default_branch":`).str(r.DefaultBranch)
	w.raw(`,"created_at":`).str(isoTime(r.CreatedAt))
	w.raw(`,"updated_at":`).str(isoTime(r.CreatedAt))
	w.raw("}\n")
	r.jsonCache = w.bytes()
	return r.jsonCache
}

func userID(login string) int {
	if u, ok := usersByLogin[login]; ok {
		return u.ID
	}
	return 0
}

func issueJSON(is *Issue) []byte {
	if is.jsonCache != nil {
		return is.jsonCache
	}
	w := &jw{}
	w.raw(`{"id":`).num(is.ID)
	w.raw(`,"number":`).num(is.Number)
	w.raw(`,"title":`).str(is.Title)
	w.raw(`,"body":`).str(is.Body)
	w.raw(`,"state":`).str(is.State)
	w.raw(`,"labels":[`)
	for i, name := range is.Labels {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"name":`).str(name).raw(`}`)
	}
	w.raw(`],"user":{"login":`).str(viewerUser.Login).raw(`}`)
	w.raw(`,"comments":`).num(len(is.Comments))
	w.raw(`,"created_at":`).str(isoTime(is.CreatedAt))
	w.raw(`,"updated_at":`).str(isoTime(is.CreatedAt))
	w.raw("}\n")
	is.jsonCache = w.bytes()
	return is.jsonCache
}

func commentJSON(c *Comment) []byte {
	w := &jw{}
	w.raw(`{"id":`).num(c.ID)
	w.raw(`,"body":`).str(c.Body)
	w.raw(`,"user":{"login":`).str(viewerUser.Login).raw(`}`)
	w.raw(`,"created_at":`).str(isoTime(c.CreatedAt))
	w.raw("}\n")
	return w.bytes()
}

func (r *Repo) refTip(ref string) string {
	if oid, ok := r.Refs[ref]; ok {
		return oid
	}
	return ""
}

func pullJSON(r *Repo, p *Pull) []byte {
	w := &jw{}
	w.raw(`{"id":`).num(p.ID)
	w.raw(`,"number":`).num(p.Number)
	w.raw(`,"state":`).str(p.State)
	w.raw(`,"title":`).str(p.Title)
	w.raw(`,"body":`).str(p.Body)
	w.raw(`,"user":{"login":`).str(viewerUser.Login).raw(`}`)
	w.raw(`,"head":{"ref":`).str(p.HeadRef).raw(`,"sha":`).str(r.refTip("refs/heads/" + p.HeadRef)).raw(`}`)
	w.raw(`,"base":{"ref":`).str(p.BaseRef).raw(`,"sha":`).str(r.refTip("refs/heads/" + p.BaseRef)).raw(`}`)
	w.raw(`,"merged":`).boolean(p.Merged)
	if p.Merged {
		w.raw(`,"merged_at":`).str(p.MergedAt)
		w.raw(`,"merge_commit_sha":`).str(p.MergeCommitSHA)
	} else {
		w.raw(`,"merged_at":null,"merge_commit_sha":null`)
	}
	w.raw(`,"created_at":`).str(isoTime(p.CreatedAt))
	w.raw("}\n")
	return w.bytes()
}

// ---- routing ----

func handleREST(c *reqCtx) {
	w, r := c.w, c.r
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")

	// The API root is public, as on api.github.com.
	if r.Method == http.MethodGet && (path == "" || path == "/") {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		sendJSON(w, http.StatusOK, []byte("{\"current_user_url\":\"/api/v3/user\",\"rate_limit_url\":\"/api/v3/rate_limit\"}\n"))
		return
	}
	if !authorised(r) {
		sendError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}

	seg := strings.Split(strings.Trim(path, "/"), "/")
	if path == "/" {
		seg = nil
	}

	switch {
	case r.Method == http.MethodGet && path == "/meta":
		sendJSON(w, http.StatusOK, []byte("{\"installed_version\":\"lucky-forge 1.0.0\"}\n"))
	case r.Method == http.MethodGet && path == "/user":
		w2 := &jw{}
		w2.raw(`{"login":`).str(viewerUser.Login)
		w2.raw(`,"id":`).num(viewerUser.ID)
		w2.raw(`,"name":`).str(viewerUser.Name)
		w2.raw(`,"email":`).str(viewerUser.Email)
		w2.raw(`,"type":"User"}` + "\n")
		sendJSON(w, http.StatusOK, w2.bytes())
	case r.Method == http.MethodPost && path == "/user/repos":
		restCreateRepo(c)
	case r.Method == http.MethodGet && path == "/user/repos":
		restListUserRepos(c)
	case len(seg) >= 3 && seg[0] == "repos":
		restRepo(c, seg[1], seg[2], seg[3:])
	default:
		sendError(w, http.StatusNotFound, "Not Found")
	}
}

func restCreateRepo(c *reqCtx) {
	body, ok := c.jsonBody()
	if !ok {
		sendError(c.w, http.StatusBadRequest, "Malformed JSON")
		return
	}
	name, _ := strField(body, "name")
	if name == "" {
		sendError(c.w, http.StatusUnprocessableEntity, "Validation Failed: name is required")
		return
	}
	private, _ := boolField(body, "private")
	description, _ := strField(body, "description")
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.repo(viewerUser.Login, name) != nil {
		sendError(c.w, http.StatusUnprocessableEntity, "Repository creation failed: name already exists on this account")
		return
	}
	repo := store.createRepo(viewerUser.Login, name, private, description)
	sendJSON(c.w, http.StatusCreated, repoJSON(repo))
}

func restListUserRepos(c *reqCtx) {
	r := c.r
	q := r.URL.Query()
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	if perPage <= 0 {
		perPage = 30
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	var mine []*Repo
	for _, repo := range store.order {
		if repo.Owner == viewerUser.Login {
			mine = append(mine, repo)
		}
	}
	start := (page - 1) * perPage
	if start > len(mine) {
		start = len(mine)
	}
	end := start + perPage
	if end > len(mine) {
		end = len(mine)
	}
	w := &jw{}
	w.raw("[")
	for i, repo := range mine[start:end] {
		if i > 0 {
			w.raw(",")
		}
		w.raw(strings.TrimRight(string(repoJSON(repo)), "\n"))
	}
	w.raw("]\n")
	if end < len(mine) {
		next := fmt.Sprintf("http://%s/api/v3/user/repos?per_page=%d&page=%d", r.Host, perPage, page+1)
		c.w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	sendJSON(c.w, http.StatusOK, w.bytes())
}

// restRepo handles everything under /repos/{owner}/{name}/...
func restRepo(c *reqCtx, owner, name string, rest []string) {
	w, r := c.w, c.r
	store.mu.Lock()
	defer store.mu.Unlock()
	repo := store.repo(owner, name)
	if repo == nil {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			sendJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodPatch:
			body, ok := c.jsonBody()
			if !ok {
				sendError(w, http.StatusBadRequest, "Malformed JSON")
				return
			}
			if v, ok := strField(body, "description"); ok {
				repo.Description = v
			}
			if v, ok := boolField(body, "private"); ok {
				repo.Private = v
			}
			if v, ok := strField(body, "default_branch"); ok && v != "" {
				repo.DefaultBranch = v
				repo.HeadRef = "refs/heads/" + v
			}
			if v, ok := strField(body, "name"); ok && v != "" && v != repo.Name {
				delete(store.repos, repoKey(repo.Owner, repo.Name))
				repo.Name = v
				store.repos[repoKey(repo.Owner, repo.Name)] = repo
			}
			repo.jsonCache = nil
			sendJSON(w, http.StatusOK, repoJSON(repo))
		case http.MethodDelete:
			store.deleteRepo(owner, name)
			w.WriteHeader(http.StatusNoContent)
		default:
			sendError(w, http.StatusNotFound, "Not Found")
		}
		return
	}

	switch rest[0] {
	case "branches":
		if r.Method == http.MethodGet && len(rest) == 1 {
			restListBranches(c, repo)
			return
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" && r.Method == http.MethodPost {
			restCreateRef(c, repo)
			return
		}
	case "commits":
		if r.Method == http.MethodGet && len(rest) == 1 {
			restListCommits(c, repo)
			return
		}
	case "contents":
		restContents(c, repo, rest[1:])
		return
	case "labels":
		restLabels(c, repo, rest[1:])
		return
	case "issues":
		restIssues(c, repo, rest[1:])
		return
	case "pulls":
		restPulls(c, repo, rest[1:])
		return
	}
	sendError(w, http.StatusNotFound, "Not Found")
}

func restListBranches(c *reqCtx, repo *Repo) {
	var names []string
	for ref := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	w := &jw{}
	w.raw("[")
	for i, n := range names {
		if i > 0 {
			w.raw(",")
		}
		w.raw(`{"name":`).str(n).raw(`,"commit":{"sha":`).str(repo.Refs["refs/heads/"+n]).raw(`}}`)
	}
	w.raw("]\n")
	sendJSON(c.w, http.StatusOK, w.bytes())
}

func restCreateRef(c *reqCtx, repo *Repo) {
	body, ok := c.jsonBody()
	if !ok {
		sendError(c.w, http.StatusBadRequest, "Malformed JSON")
		return
	}
	ref, _ := strField(body, "ref")
	sha, _ := strField(body, "sha")
	if ref == "" || sha == "" || repo.getObject(sha) == nil {
		sendError(c.w, http.StatusUnprocessableEntity, "Validation Failed: ref and a known sha are required")
		return
	}
	repo.Refs[ref] = sha
	typ := repo.getObject(sha).Type
	w := &jw{}
	w.raw(`{"ref":`).str(ref).raw(`,"object":{"sha":`).str(sha).raw(`,"type":`).str(typ).raw(`}}` + "\n")
	sendJSON(c.w, http.StatusCreated, w.bytes())
}

// walkCommits lists commits reachable from tip, newest first (DFS pre-order
// following parents in order, duplicates removed).
func walkCommits(repo *Repo, tip string) []string {
	var out []string
	seen := map[string]bool{}
	var visit func(oid string)
	visit = func(oid string) {
		if oid == "" || seen[oid] {
			return
		}
		obj := repo.getObject(oid)
		if obj == nil || obj.Type != "commit" {
			return
		}
		seen[oid] = true
		out = append(out, oid)
		c := parseCommit(obj.Data)
		for _, p := range c.Parents {
			visit(p)
		}
	}
	visit(tip)
	return out
}

func restListCommits(c *reqCtx, repo *Repo) {
	branch := c.r.URL.Query().Get("sha")
	var tip string
	if branch == "" {
		tip = repo.Refs[repo.HeadRef]
	} else if oid, ok := repo.Refs["refs/heads/"+branch]; ok {
		tip = oid
	} else if _, ok2 := repo.Refs[branch]; ok2 {
		tip = repo.Refs[branch]
	} else if repo.getObject(branch) != nil {
		tip = branch
	}
	w := &jw{}
	w.raw("[")
	for i, oid := range walkCommits(repo, tip) {
		if i > 0 {
			w.raw(",")
		}
		cm := parseCommit(repo.getObject(oid).Data)
		ts := commitTimestamp(cm.Author)
		date := time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05Z")
		w.raw(`{"sha":`).str(oid)
		w.raw(`,"commit":{"message":`).str(cm.Message)
		w.raw(`,"author":{"name":`).str(identName(cm.Author)).raw(`,"email":`).str(identEmail(cm.Author)).raw(`,"date":`).str(date).raw(`}`)
		w.raw(`,"committer":{"name":`).str(identName(cm.Committer)).raw(`,"email":`).str(identEmail(cm.Committer)).raw(`,"date":`).str(date).raw(`}`)
		w.raw(`,"tree":{"sha":`).str(cm.Tree).raw(`}}`)
		w.raw(`,"parents":[`)
		for j, p := range cm.Parents {
			if j > 0 {
				w.raw(",")
			}
			w.raw(`{"sha":`).str(p).raw(`}`)
		}
		w.raw(`]}`)
	}
	w.raw("]\n")
	sendJSON(c.w, http.StatusOK, w.bytes())
}

func identName(ident string) string {
	name, _, _ := strings.Cut(ident, " <")
	return name
}

func identEmail(ident string) string {
	_, rest, ok := strings.Cut(ident, "<")
	if !ok {
		return ""
	}
	email, _, _ := strings.Cut(rest, ">")
	return email
}
