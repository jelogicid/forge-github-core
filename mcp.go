package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

func rpcWrite(w http.ResponseWriter, status int, id json.RawMessage, result any, rpcErr map[string]any) {
	body := map[string]any{"jsonrpc": "2.0"}
	if len(id) == 0 {
		body["id"] = nil
	} else {
		body["id"] = id
	}
	if rpcErr != nil {
		body["error"] = rpcErr
	} else {
		body["result"] = result
	}
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

func rpcResult(w http.ResponseWriter, id json.RawMessage, result any) {
	rpcWrite(w, 200, id, result, nil)
}

func rpcError(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	rpcWrite(w, status, id, nil, map[string]any{"code": code, "message": msg})
}

func mcpTools() []map[string]any {
	obj := func(props map[string]any, required []string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required}
	}
	s := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{"name": "create_repo", "description": "Create a new repository for the authenticated user.",
			"inputSchema": obj(map[string]any{"name": s("Repository name"), "description": s("Repository description"), "private": map[string]any{"type": "boolean", "description": "Whether the repository is private"}}, []string{"name"})},
		{"name": "get_repo", "description": "Get a repository by owner and name.",
			"inputSchema": obj(map[string]any{"owner": s("Repository owner login"), "repo": s("Repository name")}, []string{"owner", "repo"})},
		{"name": "create_issue", "description": "Create an issue in a repository.",
			"inputSchema": obj(map[string]any{"owner": s("Repository owner"), "repo": s("Repository name"), "title": s("Issue title"), "body": s("Issue body")}, []string{"owner", "repo", "title"})},
		{"name": "list_issues", "description": "List issues in a repository.",
			"inputSchema": obj(map[string]any{"owner": s("Repository owner"), "repo": s("Repository name"), "state": s("Issue state filter: open, closed, all")}, []string{"owner", "repo"})},
		{"name": "create_pull_request", "description": "Open a pull request from a head branch into a base branch.",
			"inputSchema": obj(map[string]any{"owner": s("Repository owner"), "repo": s("Repository name"), "title": s("Pull request title"), "body": s("Pull request body"), "head": s("Head branch"), "base": s("Base branch")}, []string{"owner", "repo", "title", "head", "base"})},
		{"name": "merge_pull_request", "description": "Merge an open pull request.",
			"inputSchema": obj(map[string]any{"owner": s("Repository owner"), "repo": s("Repository name"), "pull_number": map[string]any{"type": "integer", "description": "Pull request number"}}, []string{"owner", "repo", "pull_number"})},
	}
}

func (a *App) serveMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == "DELETE" {
		w.WriteHeader(200)
		return
	}
	if r.Method != "POST" {
		rpcError(w, 405, nil, -32600, "Only POST is supported")
		return
	}
	if !a.authed(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	data, _ := readBody(r)
	var req rpcReq
	if err := json.Unmarshal(data, &req); err != nil {
		rpcError(w, 400, nil, -32700, "Parse error")
		return
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		w.WriteHeader(202)
		return
	}
	switch req.Method {
	case "server/discover":
		rpcError(w, 200, req.ID, -32601, "Method not found")
	case "initialize":
		pv := "2025-11-25"
		if v, ok := req.Params["protocolVersion"].(string); ok && v != "" {
			pv = v
		}
		w.Header().Set("Mcp-Session-Id", "forge-session-1")
		rpcResult(w, req.ID, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "forge-mcp", "version": "1.0.0"},
		})
	case "ping":
		rpcResult(w, req.ID, map[string]any{})
	case "tools/list":
		rpcResult(w, req.ID, map[string]any{"tools": mcpTools()})
	case "tools/call":
		a.mcpCall(w, req)
	default:
		rpcError(w, 200, req.ID, -32601, "Method not found: "+req.Method)
	}
}

func mcpToolResult(obj any, isErr bool) map[string]any {
	text, _ := json.Marshal(obj)
	return map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": obj,
		"isError":           isErr,
	}
}

func argStr(args map[string]any, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func (a *App) mcpCall(w http.ResponseWriter, req rpcReq) {
	name := argStr(req.Params, "name")
	args, _ := req.Params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	toolErr := func(msg string) { rpcResult(w, req.ID, mcpToolResult(map[string]any{"error": msg}, true)) }

	switch name {
	case "create_repo":
		rn := argStr(args, "name")
		if rn == "" {
			toolErr("missing required argument: name")
			return
		}
		if a.getRepo(viewerLogin, rn) != nil {
			toolErr("repository already exists")
			return
		}
		priv, _ := args["private"].(bool)
		repo := a.createRepo(rn, argStr(args, "description"), priv, "")
		rpcResult(w, req.ID, mcpToolResult(a.repoObj(repo), false))
	case "get_repo":
		owner, rn := argStr(args, "owner"), argStr(args, "repo")
		if owner == "" || rn == "" {
			toolErr("missing required arguments: owner, repo")
			return
		}
		repo := a.getRepo(owner, rn)
		if repo == nil {
			toolErr("repository not found")
			return
		}
		rpcResult(w, req.ID, mcpToolResult(a.repoObj(repo), false))
	case "create_issue":
		owner, rn, title := argStr(args, "owner"), argStr(args, "repo"), argStr(args, "title")
		if owner == "" || rn == "" || title == "" {
			toolErr("missing required arguments: owner, repo, title")
			return
		}
		repo := a.getRepo(owner, rn)
		if repo == nil {
			toolErr("repository not found")
			return
		}
		repo.issueSeq++
		_, ts := a.nextTime()
		it := &Issue{Number: repo.issueSeq, Title: title, Body: argStr(args, "body"), State: "open", CreatedAt: ts, UpdatedAt: ts}
		repo.Issues = append(repo.Issues, it)
		rpcResult(w, req.ID, mcpToolResult(a.issueObj(repo, it), false))
	case "list_issues":
		owner, rn := argStr(args, "owner"), argStr(args, "repo")
		if owner == "" || rn == "" {
			toolErr("missing required arguments: owner, repo")
			return
		}
		repo := a.getRepo(owner, rn)
		if repo == nil {
			toolErr("repository not found")
			return
		}
		state := argStr(args, "state")
		if state == "" {
			state = "open"
		}
		var out []map[string]any
		for _, it := range repo.Issues {
			if state == "all" || it.State == state {
				out = append(out, a.issueObj(repo, it))
			}
		}
		if out == nil {
			out = []map[string]any{}
		}
		rpcResult(w, req.ID, mcpToolResult(map[string]any{"issues": out}, false))
	case "create_pull_request":
		owner, rn, title, head, base := argStr(args, "owner"), argStr(args, "repo"), argStr(args, "title"), argStr(args, "head"), argStr(args, "base")
		if owner == "" || rn == "" || title == "" || head == "" || base == "" {
			toolErr("missing required arguments: owner, repo, title, head, base")
			return
		}
		repo := a.getRepo(owner, rn)
		if repo == nil {
			toolErr("repository not found")
			return
		}
		headSHA := repo.tip("refs/heads/" + head)
		baseSHA := repo.tip("refs/heads/" + base)
		if headSHA == "" {
			toolErr("head branch not found")
			return
		}
		repo.pullSeq++
		_, ts := a.nextTime()
		p := &Pull{Number: repo.pullSeq, Title: title, Body: argStr(args, "body"), State: "open", HeadRef: head, HeadSHA: headSHA, BaseRef: base, BaseSHA: baseSHA, CreatedAt: ts, UpdatedAt: ts}
		repo.Pulls = append(repo.Pulls, p)
		rpcResult(w, req.ID, mcpToolResult(a.pullObj(repo, p), false))
	case "merge_pull_request":
		owner, rn := argStr(args, "owner"), argStr(args, "repo")
		num := 0
		switch v := args["pull_number"].(type) {
		case float64:
			num = int(v)
		case string:
			num, _ = strconv.Atoi(v)
		}
		if owner == "" || rn == "" || num == 0 {
			toolErr("missing required arguments: owner, repo, pull_number")
			return
		}
		repo := a.getRepo(owner, rn)
		if repo == nil {
			toolErr("repository not found")
			return
		}
		p := a.findPull(repo, num)
		if p == nil {
			toolErr("pull request not found")
			return
		}
		baseRef := "refs/heads/" + p.BaseRef
		headRef := "refs/heads/" + p.HeadRef
		baseSHA := repo.tip(baseRef)
		headSHA := repo.tip(headRef)
		if a.conflicted(repo, baseSHA, headSHA) {
			toolErr("merge conflict")
			return
		}
		headFiles := repo.filesAt(headRef)
		tree := repo.buildTreeRecursive(headFiles)
		ep, ts := a.nextTime()
		id := identity(viewerName, viewerEmail, ep)
		c := commitInfo{Tree: tree, Parents: []string{baseSHA, headSHA}, Author: id, Committer: id, Message: "Merge pull request #" + strconv.Itoa(p.Number)}
		newSHA := repo.putObj("commit", buildCommit(c))
		repo.Refs[baseRef] = newSHA
		p.Merged = true
		p.State = "closed"
		p.MergeCommitSHA = newSHA
		p.MergedAt = ts
		p.UpdatedAt = ts
		rpcResult(w, req.ID, mcpToolResult(map[string]any{"sha": newSHA, "merged": true, "message": "Pull Request successfully merged"}, false))
	default:
		rpcError(w, 200, req.ID, -32602, "Unknown tool: "+name)
	}
}