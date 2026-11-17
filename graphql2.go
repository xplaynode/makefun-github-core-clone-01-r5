package main

// The GraphQL executor: schema field tables (which also drive __type
// introspection, so the two can never drift apart), resolvers over the
// store, cursor connections, and the HTTP handler with batching.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// typeFields is the schema: type name -> ordered field names. The
// introspected counts are part of the contract (Issue 6, PullRequest 11,
// Repository 14), so keep these lists exact.
var typeFields = map[string][]string{
	"Query":                      {"viewer", "repository", "node", "search"},
	"Mutation":                   {"createIssue", "closeIssue", "createPullRequest", "mergePullRequest", "addComment"},
	"User":                       {"id", "databaseId", "login", "name", "email", "url", "isViewer"},
	"Repository":                 {"id", "databaseId", "name", "nameWithOwner", "description", "isPrivate", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequest", "pullRequests"},
	"Issue":                      {"id", "number", "title", "state", "body", "comments"},
	"PullRequest":                {"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "author"},
	"Comment":                    {"id", "body", "author", "createdAt"},
	"Label":                      {"id", "name"},
	"Ref":                        {"id", "name", "prefix", "target"},
	"Commit":                     {"id", "oid"},
	"PageInfo":                   {"hasNextPage", "hasPreviousPage", "startCursor", "endCursor"},
	"IssueConnection":            {"edges", "nodes", "pageInfo", "totalCount"},
	"IssueEdge":                  {"cursor", "node"},
	"PullRequestConnection":      {"edges", "nodes", "pageInfo", "totalCount"},
	"PullRequestEdge":            {"cursor", "node"},
	"CommentConnection":          {"edges", "nodes", "pageInfo", "totalCount"},
	"CommentEdge":                {"cursor", "node"},
	"PullRequestFileConnection":  {"edges", "nodes", "pageInfo", "totalCount"},
	"PullRequestFileEdge":        {"cursor", "node"},
	"PullRequestFile":            {"path", "additions", "deletions", "changeType"},
	"CreateIssuePayload":         {"issue"},
	"CloseIssuePayload":          {"issue"},
	"CreatePullRequestPayload":   {"pullRequest"},
	"MergePullRequestPayload":    {"pullRequest"},
	"AddCommentPayload":          {"comment", "commentEdge"},
	"SearchResultItemConnection": {"edges", "nodes", "pageInfo", "issueCount", "repositoryCount"},
	"__Type":                     {"name", "kind", "fields", "enumValues", "description"},
	"__Field":                    {"name", "description"},
	"__EnumValue":                {"name", "description"},
	"__Schema":                   {"queryType", "mutationType", "types"},
}

var enumTypes = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "CODE"},
}

func typeHasField(typ, field string) bool {
	if field == "__typename" {
		return true
	}
	for _, f := range typeFields[typ] {
		if f == field {
			return true
		}
	}
	return false
}

// gobj is a GraphQL object value mid-resolution.
type gobj struct {
	typ     string
	repo    *Repo
	issue   *Issue
	pull    *Pull
	comment *Comment
	user    *User
	// connection payload
	items   []*gobj
	total   int
	hasNext bool
	start   int
	// edge payload
	node   *gobj
	cursor string
	// misc
	str      string
	num      int
	fc       *fileChange
	typeName string // for __Type objects
}

type gqlError struct {
	message string
}

type executor struct {
	fragments map[string][]*gqlField
	errs      []gqlError
}

func (e *executor) addErr(format string, args ...any) {
	e.errs = append(e.errs, gqlError{message: fmt.Sprintf(format, args...)})
}

func argStr(args map[string]any, key string) string {
	if args == nil {
		return ""
	}
	switch v := args[key].(type) {
	case string:
		return v
	case gqlEnum:
		return string(v)
	}
	return ""
}

func argInt(args map[string]any, key string, def int) int {
	if args == nil {
		return def
	}
	switch v := args[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

func argStrList(args map[string]any, key string) []string {
	if args == nil {
		return nil
	}
	var out []string
	switch v := args[key].(type) {
	case []any:
		for _, item := range v {
			switch s := item.(type) {
			case string:
				out = append(out, s)
			case gqlEnum:
				out = append(out, string(s))
			}
		}
	case string:
		out = append(out, v)
	case gqlEnum:
		out = append(out, string(v))
	}
	return out
}

func inputOf(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	if in, ok := args["input"].(map[string]any); ok {
		return in
	}
	return args
}

// execSelection resolves a selection set against obj into a JSON-ready map.
func (e *executor) execSelection(obj *gobj, sel []*gqlField) map[string]any {
	out := map[string]any{}
	for _, f := range sel {
		if f.name == "" {
			// Inline fragment group: merge its fields.
			for k, v := range e.execSelection(obj, f.children) {
				out[k] = v
			}
			continue
		}
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out[key] = obj.typ
			continue
		}
		if !typeHasField(obj.typ, f.name) {
			e.addErr("Field '%s' doesn't exist on type '%s'", f.name, obj.typ)
			out[key] = nil
			continue
		}
		val := e.resolve(obj, f)
		out[key] = e.shape(val, f)
	}
	return out
}

// shape applies the field's sub-selection to a resolved value.
func (e *executor) shape(val any, f *gqlField) any {
	switch v := val.(type) {
	case nil:
		return nil
	case *pageInfoValue:
		if len(f.children) == 0 {
			return v.m
		}
		out := map[string]any{}
		for _, ch := range f.children {
			if fv, ok := v.m[ch.name]; ok {
				out[fieldKey(ch)] = fv
			}
		}
		return out
	case *gobj:
		if v == nil {
			return nil
		}
		if len(f.children) == 0 {
			return nil
		}
		return e.execSelection(v, f.children)
	case []*gobj:
		out := make([]any, 0, len(v))
		for _, item := range v {
			if item == nil {
				out = append(out, nil)
			} else if len(f.children) == 0 {
				out = append(out, nil)
			} else {
				out = append(out, e.execSelection(item, f.children))
			}
		}
		return out
	default:
		return val
	}
}

// connectionOf pages items into a connection object.
func connectionOf(typ, edgeTyp string, all []*gobj, args map[string]any) *gobj {
	first := argInt(args, "first", len(all))
	after := argStr(args, "after")
	start := 0
	if after != "" {
		if raw, err := base64.StdEncoding.DecodeString(after); err == nil {
			if n, err := strconv.Atoi(string(raw)); err == nil {
				start = n + 1
			}
		}
	}
	if start > len(all) {
		start = len(all)
	}
	end := start + first
	if end > len(all) {
		end = len(all)
	}
	conn := &gobj{typ: typ, total: len(all), hasNext: end < len(all), start: start}
	for i := start; i < end; i++ {
		conn.items = append(conn.items, all[i])
	}
	_ = edgeTyp
	return conn
}

func cursorFor(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(idx)))
}

// resolve computes one field of obj.
func (e *executor) resolve(obj *gobj, f *gqlField) any {
	args := f.args
	switch obj.typ {
	case "Query":
		switch f.name {
		case "viewer":
			return &gobj{typ: "User", user: viewerUser}
		case "repository":
			owner := argStr(args, "owner")
			name := argStr(args, "name")
			if r := store.repo(owner, name); r != nil {
				return &gobj{typ: "Repository", repo: r}
			}
			return nil
		case "node":
			return e.resolveNode(argStr(args, "id"))
		case "search":
			return e.resolveSearch(args)
		}
	case "Mutation":
		return e.resolveMutation(f)
	case "User":
		switch f.name {
		case "id":
			return globalID("User", obj.user.ID)
		case "databaseId":
			return obj.user.ID
		case "login":
			return obj.user.Login
		case "name":
			return obj.user.Name
		case "email":
			return obj.user.Email
		case "url":
			return "/" + obj.user.Login
		case "isViewer":
			return obj.user.Login == viewerUser.Login
		}
	case "Repository":
		return e.resolveRepository(obj, f)
	case "Issue":
		is := obj.issue
		switch f.name {
		case "id":
			return globalID("Issue", is.ID)
		case "number":
			return is.Number
		case "title":
			return is.Title
		case "state":
			if is.State == "closed" {
				return "CLOSED"
			}
			return "OPEN"
		case "body":
			return is.Body
		case "comments":
			var items []*gobj
			for _, cm := range is.Comments {
				items = append(items, &gobj{typ: "Comment", comment: cm})
			}
			return connectionOf("CommentConnection", "CommentEdge", items, args)
		}
	case "Comment":
		switch f.name {
		case "id":
			return globalID("Comment", obj.comment.ID)
		case "body":
			return obj.comment.Body
		case "author":
			return &gobj{typ: "User", user: viewerUser}
		case "createdAt":
			return isoTime(obj.comment.CreatedAt)
		}
	case "PullRequest":
		return e.resolvePull(obj, f)
	case "Ref":
		switch f.name {
		case "id":
			return globalID("Ref", obj.num)
		case "name":
			return obj.str
		case "prefix":
			return "refs/heads/"
		case "target":
			return &gobj{typ: "Commit", str: obj.cursor}
		}
	case "Commit":
		switch f.name {
		case "id":
			return globalID("Commit", obj.num)
		case "oid":
			return obj.str
		}
	case "PageInfo":
		// handled via connection fields below
	case "IssueConnection", "PullRequestConnection", "CommentConnection", "PullRequestFileConnection", "SearchResultItemConnection":
		return e.resolveConnection(obj, f)
	case "IssueEdge", "PullRequestEdge", "CommentEdge", "PullRequestFileEdge":
		switch f.name {
		case "cursor":
			return obj.cursor
		case "node":
			return obj.node
		}
	case "PullRequestFile":
		switch f.name {
		case "path":
			return obj.fc.Path
		case "additions":
			return obj.fc.Additions
		case "deletions":
			return obj.fc.Deletions
		case "changeType":
			switch obj.fc.Status {
			case "added":
				return "ADDED"
			case "removed":
				return "DELETED"
			default:
				return "MODIFIED"
			}
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		switch f.name {
		case "issue":
			if obj.issue != nil {
				return &gobj{typ: "Issue", issue: obj.issue, repo: obj.repo}
			}
			return nil
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		switch f.name {
		case "pullRequest":
			if obj.pull != nil {
				return &gobj{typ: "PullRequest", pull: obj.pull, repo: obj.repo}
			}
			return nil
		}
	case "AddCommentPayload":
		switch f.name {
		case "comment":
			if obj.comment != nil {
				return &gobj{typ: "Comment", comment: obj.comment}
			}
			return nil
		case "commentEdge":
			return &gobj{typ: "CommentEdge", node: &gobj{typ: "Comment", comment: obj.comment}, cursor: cursorFor(0)}
		}
	case "__Type":
		return e.resolveType(obj, f)
	case "__Field":
		return obj.str // name only usage; see resolveType
	case "__Schema":
		switch f.name {
		case "queryType":
			return &gobj{typ: "__Type", typeName: "Query"}
		case "mutationType":
			return &gobj{typ: "__Type", typeName: "Mutation"}
		case "types":
			var out []*gobj
			for name := range typeFields {
				out = append(out, &gobj{typ: "__Type", typeName: name})
			}
			for name := range enumTypes {
				out = append(out, &gobj{typ: "__Type", typeName: name})
			}
			return out
		}
	}
	return nil
}

func (e *executor) resolveType(obj *gobj, f *gqlField) any {
	name := obj.typeName
	switch f.name {
	case "name":
		return name
	case "kind":
		if _, ok := enumTypes[name]; ok {
			return "ENUM"
		}
		return "OBJECT"
	case "description":
		return nil
	case "fields":
		fields := typeFields[name]
		if fields == nil {
			return nil
		}
		out := make([]any, 0, len(fields))
		for _, fn := range fields {
			out = append(out, map[string]any{"name": fn})
		}
		return out
	case "enumValues":
		vals := enumTypes[name]
		if vals == nil {
			return nil
		}
		out := make([]any, 0, len(vals))
		for _, v := range vals {
			out = append(out, map[string]any{"name": v})
		}
		return out
	}
	return nil
}

func (e *executor) resolveConnection(obj *gobj, f *gqlField) any {
	switch f.name {
	case "totalCount", "issueCount":
		return obj.total
	case "repositoryCount":
		return 0
	case "nodes":
		return obj.items
	case "pageInfo":
		end := obj.start + len(obj.items) - 1
		pi := &gobj{typ: "PageInfo"}
		_ = pi
		m := map[string]any{
			"hasNextPage":     obj.hasNext,
			"hasPreviousPage": obj.start > 0,
			"startCursor":     nil,
			"endCursor":       nil,
		}
		if len(obj.items) > 0 {
			m["startCursor"] = cursorFor(obj.start)
			m["endCursor"] = cursorFor(end)
		}
		// PageInfo is a leaf map; shape() passes maps through.
		return &pageInfoValue{m: m}
	case "edges":
		edgeTyp := strings.TrimSuffix(obj.typ, "Connection") + "Edge"
		out := make([]*gobj, 0, len(obj.items))
		for i, item := range obj.items {
			out = append(out, &gobj{typ: edgeTyp, node: item, cursor: cursorFor(obj.start + i)})
		}
		return out
	}
	return nil
}

// pageInfoValue wraps a pre-shaped map so shape() can pass it through while
// still applying sub-selections trivially (all PageInfo fields are scalars).
type pageInfoValue struct{ m map[string]any }

// orderDirection reads args.orderBy.direction ("ASC" default, "DESC").
func orderDirection(args map[string]any) string {
	if args == nil {
		return "ASC"
	}
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if d := argStr(ob, "direction"); d != "" {
			return d
		}
	}
	return "ASC"
}

func (e *executor) resolveRepository(obj *gobj, f *gqlField) any {
	r := obj.repo
	args := f.args
	switch f.name {
	case "id":
		return globalID("Repository", r.ID)
	case "databaseId":
		return r.ID
	case "name":
		return r.Name
	case "nameWithOwner":
		return r.Owner + "/" + r.Name
	case "description":
		return r.Description
	case "isPrivate":
		return r.Private
	case "owner":
		owner := viewerUser
		if u, ok := usersByLogin[r.Owner]; ok {
			owner = u
		}
		return &gobj{typ: "User", user: owner}
	case "defaultBranchRef":
		tip := r.Refs[r.HeadRef]
		if tip == "" {
			return nil
		}
		return &gobj{typ: "Ref", str: r.DefaultBranch, cursor: tip, num: r.ID}
	case "viewerPermission":
		return "ADMIN"
	case "hasIssuesEnabled":
		return true
	case "issue":
		num := argInt(args, "number", 0)
		if is := r.Issues[num]; is != nil {
			return &gobj{typ: "Issue", issue: is, repo: r}
		}
		return nil
	case "issues":
		states := argStrList(args, "states")
		var items []*gobj
		var nums []int
		for n := range r.Issues {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			is := r.Issues[n]
			if len(states) > 0 {
				want := "OPEN"
				if is.State == "closed" {
					want = "CLOSED"
				}
				found := false
				for _, s := range states {
					if s == want {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			items = append(items, &gobj{typ: "Issue", issue: is, repo: r})
		}
		if orderDirection(args) == "DESC" {
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}
		return connectionOf("IssueConnection", "IssueEdge", items, args)
	case "pullRequest":
		num := argInt(args, "number", 0)
		if p := r.Pulls[num]; p != nil {
			return &gobj{typ: "PullRequest", pull: p, repo: r}
		}
		return nil
	case "pullRequests":
		states := argStrList(args, "states")
		var items []*gobj
		var nums []int
		for n := range r.Pulls {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for _, n := range nums {
			p := r.Pulls[n]
			if len(states) > 0 {
				st := pullGraphQLState(p)
				found := false
				for _, s := range states {
					if s == st {
						found = true
					}
				}
				if !found {
					continue
				}
			}
			items = append(items, &gobj{typ: "PullRequest", pull: p, repo: r})
		}
		if orderDirection(args) == "DESC" {
			for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
				items[i], items[j] = items[j], items[i]
			}
		}
		return connectionOf("PullRequestConnection", "PullRequestEdge", items, args)
	}
	return nil
}

func pullGraphQLState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func (e *executor) resolvePull(obj *gobj, f *gqlField) any {
	p := obj.pull
	r := obj.repo
	switch f.name {
	case "id":
		return globalID("PullRequest", p.ID)
	case "number":
		return p.Number
	case "title":
		return p.Title
	case "body":
		return p.Body
	case "state":
		return pullGraphQLState(p)
	case "merged":
		return p.Merged
	case "mergeable":
		if p.Merged || p.State == "closed" {
			return "UNKNOWN"
		}
		if _, conflict := mergeDryRun(r, p); conflict {
			return "CONFLICTING"
		}
		return "MERGEABLE"
	case "headRefName":
		return p.HeadRef
	case "baseRefName":
		return p.BaseRef
	case "author":
		return &gobj{typ: "User", user: viewerUser}
	case "files":
		baseTree, _ := treeAtRef(r, p.BaseRef)
		headTree, _ := treeAtRef(r, p.HeadRef)
		changes := diffTrees(r, baseTree, headTree)
		var items []*gobj
		for i := range changes {
			items = append(items, &gobj{typ: "PullRequestFile", fc: &changes[i]})
		}
		return connectionOf("PullRequestFileConnection", "PullRequestFileEdge", items, f.args)
	}
	return nil
}

// mergeDryRun reports whether merging would conflict, without applying it.
func mergeDryRun(r *Repo, p *Pull) (string, bool) {
	baseTip := r.Refs["refs/heads/"+p.BaseRef]
	headTip := r.Refs["refs/heads/"+p.HeadRef]
	if baseTip == "" || headTip == "" {
		return "", true
	}
	mb := mergeBase(r, baseTip, headTip)
	mbTree, _ := treeAtRef(r, mb)
	baseTree, _ := treeAtRef(r, p.BaseRef)
	headTree, _ := treeAtRef(r, p.HeadRef)
	mbFiles := flattenTree(r, mbTree)
	baseFiles := flattenTree(r, baseTree)
	headFiles := flattenTree(r, headTree)
	for path, hf := range headFiles {
		mf, mok := mbFiles[path]
		if mok && hf.OID == mf.OID {
			continue
		}
		if bf, bok := baseFiles[path]; bok {
			if bmf, bmok := mbFiles[path]; !bmok || bf.OID != bmf.OID {
				if bf.OID != hf.OID {
					return "", true
				}
			}
		}
	}
	for path := range mbFiles {
		if _, ok := headFiles[path]; !ok {
			if bf, bok := baseFiles[path]; bok {
				if bf.OID != mbFiles[path].OID {
					return "", true
				}
			}
		}
	}
	return "", false
}

func (e *executor) resolveNode(id string) *gobj {
	typ, dbID, ok := parseGlobalID(id)
	if !ok {
		return nil
	}
	switch typ {
	case "Repository":
		if r := store.findRepoByID(dbID); r != nil {
			return &gobj{typ: "Repository", repo: r}
		}
	case "Issue":
		if r, is := store.findIssueByID(dbID); is != nil {
			return &gobj{typ: "Issue", issue: is, repo: r}
		}
	case "PullRequest":
		if r, p := store.findPullByID(dbID); p != nil {
			return &gobj{typ: "PullRequest", pull: p, repo: r}
		}
	case "User":
		for _, u := range usersByLogin {
			if u.ID == dbID {
				return &gobj{typ: "User", user: u}
			}
		}
	}
	return nil
}

func (e *executor) resolveSearch(args map[string]any) *gobj {
	typ := argStr(args, "type")
	query := argStr(args, "query")
	conn := &gobj{typ: "SearchResultItemConnection"}
	if typ == "ISSUE" || typ == "" {
		var items []*gobj
		for _, r := range store.order {
			var nums []int
			for n := range r.Issues {
				nums = append(nums, n)
			}
			sort.Ints(nums)
			for _, n := range nums {
				is := r.Issues[n]
				if query == "" || strings.Contains(is.Title, query) || strings.Contains(is.Body, query) {
					items = append(items, &gobj{typ: "Issue", issue: is, repo: r})
				}
			}
		}
		paged := connectionOf("SearchResultItemConnection", "IssueEdge", items, args)
		paged.total = len(items)
		return paged
	}
	conn.total = 0
	return conn
}

func (e *executor) resolveMutation(f *gqlField) any {
	in := inputOf(f.args)
	switch f.name {
	case "createIssue":
		_, dbID, ok := parseGlobalID(argStr(in, "repositoryId"))
		if !ok {
			e.addErr("Invalid repositoryId")
			return &gobj{typ: "CreateIssuePayload"}
		}
		r := store.findRepoByID(dbID)
		if r == nil {
			e.addErr("Repository not found")
			return &gobj{typ: "CreateIssuePayload"}
		}
		var labels []string
		for _, lid := range argStrList(in, "labelIds") {
			// Label ids are opaque here; accept "Label:<repoID>:<name>".
			if raw, err := base64.StdEncoding.DecodeString(lid); err == nil {
				parts := strings.SplitN(string(raw), ":", 3)
				if len(parts) == 3 && parts[0] == "Label" {
					labels = append(labels, parts[2])
				}
			}
		}
		labels = append(labels, argStrList(in, "labels")...)
		for _, name := range labels {
			r.addLabel(name)
		}
		is := store.createIssue(r, argStr(in, "title"), argStr(in, "body"), labels)
		return &gobj{typ: "CreateIssuePayload", issue: is, repo: r}
	case "closeIssue":
		_, dbID, ok := parseGlobalID(argStr(in, "issueId"))
		if !ok {
			e.addErr("Invalid issueId")
			return &gobj{typ: "CloseIssuePayload"}
		}
		r, is := store.findIssueByID(dbID)
		if is == nil {
			e.addErr("Issue not found")
			return &gobj{typ: "CloseIssuePayload"}
		}
		is.State = "closed"
		is.jsonCache = nil
		return &gobj{typ: "CloseIssuePayload", issue: is, repo: r}
	case "createPullRequest":
		_, dbID, ok := parseGlobalID(argStr(in, "repositoryId"))
		if !ok {
			e.addErr("Invalid repositoryId")
			return &gobj{typ: "CreatePullRequestPayload"}
		}
		r := store.findRepoByID(dbID)
		if r == nil {
			e.addErr("Repository not found")
			return &gobj{typ: "CreatePullRequestPayload"}
		}
		p := store.createPull(r, argStr(in, "title"), argStr(in, "body"), argStr(in, "headRefName"), argStr(in, "baseRefName"))
		return &gobj{typ: "CreatePullRequestPayload", pull: p, repo: r}
	case "mergePullRequest":
		_, dbID, ok := parseGlobalID(argStr(in, "pullRequestId"))
		if !ok {
			e.addErr("Invalid pullRequestId")
			return &gobj{typ: "MergePullRequestPayload"}
		}
		r, p := store.findPullByID(dbID)
		if p == nil {
			e.addErr("Pull request not found")
			return &gobj{typ: "MergePullRequestPayload"}
		}
		method := "squash"
		if strings.EqualFold(argStr(in, "mergeMethod"), "MERGE") {
			method = "merge"
		}
		mergePull(r, p, method)
		return &gobj{typ: "MergePullRequestPayload", pull: p, repo: r}
	case "addComment":
		_, dbID, ok := parseGlobalID(argStr(in, "subjectId"))
		if !ok {
			e.addErr("Invalid subjectId")
			return &gobj{typ: "AddCommentPayload"}
		}
		_, is := store.findIssueByID(dbID)
		if is == nil {
			e.addErr("Subject not found")
			return &gobj{typ: "AddCommentPayload"}
		}
		cm := store.createComment(is, argStr(in, "body"))
		is.jsonCache = nil
		return &gobj{typ: "AddCommentPayload", comment: cm}
	}
	return nil
}

// ---- HTTP ----

type gqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

func executeGraphQL(req gqlRequest) map[string]any {
	vars := req.Variables
	if vars == nil {
		vars = map[string]any{}
	}
	// Numbers in variables arrive as float64; normalise integral values.
	for k, v := range vars {
		if fv, ok := v.(float64); ok && fv == float64(int64(fv)) {
			vars[k] = int(fv)
		}
	}
	ops, fragments, err := parseDocument(req.Query, vars)
	if err != nil {
		return map[string]any{"errors": []any{map[string]any{"message": err.Error()}}}
	}
	var op *gqlOperation
	for _, o := range ops {
		if req.OperationName == "" || o.name == req.OperationName {
			op = o
			break
		}
	}
	if op == nil {
		return map[string]any{"errors": []any{map[string]any{"message": "No operation found"}}}
	}
	ex := &executor{fragments: fragments}
	root := &gobj{typ: "Query"}
	if op.typ == "mutation" {
		root = &gobj{typ: "Mutation"}
	}
	// Introspection roots are fields on Query.
	sel := expandSelections(op.selection, fragments, 0)
	data := map[string]any{}
	var execRoot func(fields []*gqlField)
	execRoot = func(fields []*gqlField) {
		for _, f := range fields {
			if f.name == "" {
				execRoot(f.children)
				continue
			}
			if f.name == "__type" {
				name := argStr(f.args, "name")
				tobj := &gobj{typ: "__Type", typeName: name}
				if _, ok := typeFields[name]; !ok {
					if _, ok2 := enumTypes[name]; !ok2 {
						data[fieldKey(f)] = nil
						continue
					}
				}
				data[fieldKey(f)] = ex.execSelection(tobj, f.children)
				continue
			}
			if f.name == "__schema" {
				data[fieldKey(f)] = ex.execSelection(&gobj{typ: "__Schema"}, f.children)
				continue
			}
			data[fieldKey(f)] = ex.shape(ex.resolveRoot(root, f), f)
		}
	}
	execRoot(sel)
	out := map[string]any{"data": data}
	if len(ex.errs) > 0 {
		errs := make([]any, 0, len(ex.errs))
		for _, ge := range ex.errs {
			errs = append(errs, map[string]any{"message": ge.message})
		}
		out["errors"] = errs
	}
	return out
}

func fieldKey(f *gqlField) string {
	if f.alias != "" {
		return f.alias
	}
	return f.name
}

// resolveRoot handles Query/Mutation root fields, including unknown-field
// errors at the root.
func (e *executor) resolveRoot(root *gobj, f *gqlField) any {
	if f.name == "__typename" {
		return root.typ
	}
	if !typeHasField(root.typ, f.name) {
		e.addErr("Field '%s' doesn't exist on type '%s'", f.name, root.typ)
		return nil
	}
	return e.resolve(root, f)
}

func handleGraphQL(c *reqCtx) {
	w, r := c.w, c.r
	if r.Method != http.MethodPost {
		sendError(w, http.StatusNotFound, "Not Found")
		return
	}
	if !authorised(r) {
		sendError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	raw := c.readBody()
	store.mu.Lock()
	defer store.mu.Unlock()
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		var batch []gqlRequest
		if err := json.Unmarshal(raw, &batch); err != nil {
			sendJSON(w, http.StatusBadRequest, []byte(`{"errors":[{"message":"Malformed JSON"}]}`))
			return
		}
		out := make([]any, 0, len(batch))
		for _, one := range batch {
			out = append(out, executeGraphQL(one))
		}
		sendJSON(w, http.StatusOK, marshalJSON(out))
		return
	}
	var req gqlRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		sendJSON(w, http.StatusBadRequest, []byte(`{"errors":[{"message":"Malformed JSON"}]}`))
		return
	}
	sendJSON(w, http.StatusOK, marshalJSON(executeGraphQL(req)))
}

// silence unused import in some build configurations.
var _ = strconv.Itoa
