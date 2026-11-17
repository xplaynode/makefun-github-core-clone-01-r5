package main

// The MCP endpoint (/mcp): a modern-era Model Context Protocol server
// discovered via server/discover, exposing six tools that operate on the
// same store as REST and GraphQL.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpTool struct {
	name        string
	description string
	schema      map[string]any
}

func mcpTools() []mcpTool {
	str := map[string]any{"type": "string"}
	return []mcpTool{
		{"create_repo", "Create a repository for the authenticated user", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        str,
				"private":     map[string]any{"type": "boolean"},
				"description": str,
			},
			"required": []string{"name"},
		}},
		{"get_repo", "Get a repository by owner and name", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": str,
				"repo":  str,
				"name":  str,
			},
			"required": []string{"repo"},
		}},
		{"create_issue", "Create an issue in a repository", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":   str,
				"owner":  str,
				"name":   str,
				"title":  str,
				"body":   str,
				"labels": map[string]any{"type": "array", "items": str},
			},
			"required": []string{"title"},
		}},
		{"list_issues", "List issues in a repository", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":  str,
				"owner": str,
				"name":  str,
				"state": str,
			},
			"required": []string{},
		}},
		{"create_pull_request", "Create a pull request", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":  str,
				"owner": str,
				"name":  str,
				"title": str,
				"body":  str,
				"head":  str,
				"base":  str,
			},
			"required": []string{"title", "head", "base"},
		}},
		{"merge_pull_request", "Merge a pull request", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":         str,
				"owner":        str,
				"name":         str,
				"number":       map[string]any{"type": "integer"},
				"merge_method": str,
			},
			"required": []string{"number"},
		}},
	}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResult(id json.RawMessage, result any) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return marshalJSON(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func rpcError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return marshalJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
}

func handleMCP(c *reqCtx) {
	w, r := c.w, c.r
	if r.Method != http.MethodPost {
		sendError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
		return
	}
	if !authorised(r) {
		sendError(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	raw := c.readBody()
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.Method == "" {
		sendJSON(w, http.StatusBadRequest, rpcError(nil, -32700, "Parse error"))
		return
	}
	// Notifications (no id) get no body back.
	isNotification := len(req.ID) == 0
	var params map[string]any
	if len(req.Params) > 0 {
		_ = json.Unmarshal(req.Params, &params)
	}
	if params == nil {
		params = map[string]any{}
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	switch req.Method {
	case "server/discover":
		serverInfo := map[string]any{"name": "lucky-forge", "version": "1.0.0"}
		result := map[string]any{
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
			"serverInfo":        serverInfo,
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
		}
		sendJSON(w, http.StatusOK, rpcResult(req.ID, result))
	case "initialize":
		protocol := "2025-11-25"
		if pv, ok := params["protocolVersion"].(string); ok {
			for _, v := range mcpSupportedVersions {
				if v == pv {
					protocol = pv
				}
			}
		}
		result := map[string]any{
			"protocolVersion": protocol,
			"serverInfo":      map[string]any{"name": "lucky-forge", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
		sendJSON(w, http.StatusOK, rpcResult(req.ID, result))
	case "ping":
		sendJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{}))
	case "tools/list":
		var tools []any
		for _, t := range mcpTools() {
			tools = append(tools, map[string]any{
				"name":        t.name,
				"description": t.description,
				"inputSchema": t.schema,
			})
		}
		sendJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{"tools": tools}))
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		result, isErr, known := callMCPTool(name, args)
		if !known {
			sendJSON(w, http.StatusOK, rpcError(req.ID, -32602, "Unknown tool: "+name))
			return
		}
		text := string(marshalJSON(result))
		sendJSON(w, http.StatusOK, rpcResult(req.ID, map[string]any{
			"content": []any{map[string]any{"type": "text", "text": text}},
			"isError": isErr,
		}))
	default:
		if isNotification {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		sendJSON(w, http.StatusOK, rpcError(req.ID, -32601, "Method not found: "+req.Method))
	}
}

// mcpRepoRef resolves the target repo from flexible argument spellings.
func mcpRepoRef(args map[string]any) (owner, name string) {
	owner = viewerUser.Login
	name = ""
	if v, ok := args["owner"].(string); ok && v != "" {
		owner = v
	}
	for _, key := range []string{"repo", "repository", "name", "repo_name"} {
		if v, ok := args[key].(string); ok && v != "" {
			name = v
			break
		}
	}
	if strings.Contains(name, "/") {
		parts := strings.SplitN(name, "/", 2)
		owner, name = parts[0], parts[1]
	}
	return owner, name
}

func toolErrorResult(msg string) (any, bool, bool) {
	return map[string]any{"error": msg}, true, true
}

func callMCPTool(name string, args map[string]any) (result any, isError bool, known bool) {
	switch name {
	case "create_repo":
		repoName, _ := args["name"].(string)
		if repoName == "" {
			return toolErrorResult("name is required")
		}
		if store.repo(viewerUser.Login, repoName) != nil {
			return toolErrorResult("Repository creation failed: name already exists on this account")
		}
		private, _ := args["private"].(bool)
		description, _ := args["description"].(string)
		repo := store.createRepo(viewerUser.Login, repoName, private, description)
		return repoResult(repo), false, true
	case "get_repo":
		owner, repoName := mcpRepoRef(args)
		repo := store.repo(owner, repoName)
		if repo == nil {
			return toolErrorResult("Not Found")
		}
		return repoResult(repo), false, true
	case "create_issue":
		owner, repoName := mcpRepoRef(args)
		title, _ := args["title"].(string)
		if title == "" || repoName == "" {
			return toolErrorResult("repo and title are required")
		}
		repo := store.repo(owner, repoName)
		if repo == nil {
			return toolErrorResult("Not Found")
		}
		bodyText, _ := args["body"].(string)
		var labels []string
		if arr, ok := args["labels"].([]any); ok {
			for _, item := range arr {
				if s, ok := item.(string); ok {
					labels = append(labels, s)
				}
			}
		}
		for _, l := range labels {
			repo.addLabel(l)
		}
		is := store.createIssue(repo, title, bodyText, labels)
		return issueResult(repo, is), false, true
	case "list_issues":
		owner, repoName := mcpRepoRef(args)
		repo := store.repo(owner, repoName)
		if repo == nil {
			return toolErrorResult("Not Found")
		}
		stateFilter, _ := args["state"].(string)
		var nums []int
		for n := range repo.Issues {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		var issues []any
		for _, n := range nums {
			is := repo.Issues[n]
			if stateFilter != "" && stateFilter != "all" && is.State != stateFilter {
				continue
			}
			issues = append(issues, issueResult(repo, is))
		}
		if issues == nil {
			issues = []any{}
		}
		return map[string]any{"issues": issues, "total": len(issues)}, false, true
	case "create_pull_request":
		owner, repoName := mcpRepoRef(args)
		title, _ := args["title"].(string)
		head, _ := args["head"].(string)
		base, _ := args["base"].(string)
		if title == "" || head == "" || base == "" || repoName == "" {
			return toolErrorResult("repo, title, head and base are required")
		}
		repo := store.repo(owner, repoName)
		if repo == nil {
			return toolErrorResult("Not Found")
		}
		head = stripOwnerPrefix(head)
		base = stripOwnerPrefix(base)
		bodyText, _ := args["body"].(string)
		p := store.createPull(repo, title, bodyText, head, base)
		return pullResult(repo, p), false, true
	case "merge_pull_request":
		owner, repoName := mcpRepoRef(args)
		repo := store.repo(owner, repoName)
		if repo == nil {
			return toolErrorResult("Not Found")
		}
		num := 0
		switch v := args["number"].(type) {
		case float64:
			num = int(v)
		case int:
			num = v
		}
		p := repo.Pulls[num]
		if p == nil {
			return toolErrorResult("Not Found")
		}
		method, _ := args["merge_method"].(string)
		sha, conflict := mergePull(repo, p, method)
		if conflict {
			return toolErrorResult("Pull Request is not mergeable")
		}
		return map[string]any{"sha": sha, "merged": true, "number": p.Number, "message": "Pull Request successfully merged"}, false, true
	}
	return nil, false, false
}

func repoResult(r *Repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          map[string]any{"login": r.Owner, "id": userID(r.Owner)},
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
	}
}

func issueResult(r *Repo, is *Issue) map[string]any {
	labels := make([]any, 0, len(is.Labels))
	for _, name := range is.Labels {
		labels = append(labels, map[string]any{"name": name})
	}
	return map[string]any{
		"id":         is.ID,
		"number":     is.Number,
		"title":      is.Title,
		"body":       is.Body,
		"state":      is.State,
		"labels":     labels,
		"repository": r.Owner + "/" + r.Name,
		"comments":   len(is.Comments),
	}
}

func pullResult(r *Repo, p *Pull) map[string]any {
	return map[string]any{
		"id":      p.ID,
		"number":  p.Number,
		"title":   p.Title,
		"body":    p.Body,
		"state":   p.State,
		"merged":  p.Merged,
		"head":    map[string]any{"ref": p.HeadRef, "sha": r.refTip("refs/heads/" + p.HeadRef)},
		"base":    map[string]any{"ref": p.BaseRef, "sha": r.refTip("refs/heads/" + p.BaseRef)},
		"message": fmt.Sprintf("pull request #%d", p.Number),
	}
}
