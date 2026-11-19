package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
)

var reqIDSeq int64

// readOnlyRequest reports whether a request only reads the store, so several
// of them (the concurrent shallow clones, the keep-alive issue reads) can run
// in parallel under the read lock while writers stay exclusive.
func readOnlyRequest(r *http.Request, path string) bool {
	if path == "/healthz" {
		return true
	}
	if strings.HasPrefix(path, "/api/v3") {
		return r.Method == "GET" || r.Method == "HEAD"
	}
	if strings.HasSuffix(path, ".git") || strings.Contains(path, ".git/") {
		if r.Method == "GET" {
			return true
		}
		return strings.Contains(path, "git-upload-pack")
	}
	return false
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if readOnlyRequest(r, path) {
		a.mu.RLock()
		defer a.mu.RUnlock()
	} else {
		a.mu.Lock()
		defer a.mu.Unlock()
	}

	if path == "/healthz" {
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	if strings.HasPrefix(path, "/api/v3") {
		a.serveREST(w, r, strings.TrimPrefix(path, "/api/v3"))
		return
	}
	if path == "/api/graphql" {
		a.serveGraphQL(w, r)
		return
	}
	if path == "/mcp" {
		a.serveMCP(w, r)
		return
	}
	if strings.HasSuffix(path, ".git") || strings.Contains(path, ".git/") {
		a.serveGit(w, r)
		return
	}
	writeJSON(w, 404, map[string]any{"message": "Not Found"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	id := atomic.AddInt64(&reqIDSeq, 1)
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-GitHub-Request-Id", fmt.Sprintf("REQ:%d", id))
	w.WriteHeader(status)
	w.Write(data)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"message": msg})
}

func (a *App) authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	return tok != "" && tok == a.token
}

func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, 16<<20))
}

func parseJSONBody(r *http.Request) (map[string]any, error) {
	data, err := readBody(r)
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func (a *App) userObj(login string) map[string]any {
	return map[string]any{
		"login": login, "id": 1000, "node_id": "U_" + login, "type": "User",
		"site_admin": false, "name": viewerName, "email": viewerEmail,
	}
}

func (a *App) repoObj(r *Repo) map[string]any {
	return map[string]any{
		"id": r.ID, "node_id": "R_" + strconv.Itoa(r.ID), "name": r.Name,
		"full_name": r.Owner + "/" + r.Name, "owner": a.userObj(r.Owner),
		"private": r.Private, "description": r.Description, "fork": false,
		"default_branch": r.DefaultBranch, "created_at": r.CreatedAt,
		"updated_at": r.CreatedAt, "pushed_at": r.CreatedAt,
		"html_url": "https://forge.local/" + r.Owner + "/" + r.Name,
		"url":      "https://forge.local/api/v3/repos/" + r.Owner + "/" + r.Name,
		"language": nil, "stargazers_count": 0, "watchers_count": 0, "forks_count": 0,
		"open_issues_count": len(r.Issues), "archived": false, "disabled": false,
		"visibility": map[bool]string{true: "private", false: "public"}[r.Private],
	}
}

func (a *App) serveREST(w http.ResponseWriter, r *http.Request, p string) {
	if !a.authed(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	method := r.Method
	if p == "" || p == "/" {
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		writeJSON(w, 200, map[string]any{
			"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit",
			"repository_url": "/api/v3/repos/{owner}/{repo}",
		})
		return
	}
	p = strings.TrimPrefix(p, "/")
	segs := strings.Split(p, "/")

	if p == "meta" {
		writeJSON(w, 200, map[string]any{"installed_version": "1.0.0", "verifiable_password_authentication": false})
		return
	}
	if p == "user" {
		writeJSON(w, 200, a.userObj(viewerLogin))
		return
	}
	if p == "user/repos" {
		if method == "POST" {
			a.restCreateRepo(w, r)
			return
		}
		if method == "GET" {
			a.restListUserRepos(w, r)
			return
		}
	}
	if len(segs) >= 2 && segs[0] == "repos" {
		a.serveRepoRoutes(w, r, segs[1], segs[2], segs[3:])
		return
	}
	writeErr(w, 404, "Not Found")
}

func (a *App) restCreateRepo(w http.ResponseWriter, r *http.Request) {
	m, err := parseJSONBody(r)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	name := str(m, "name")
	if name == "" {
		writeErr(w, 422, "Repository name is required")
		return
	}
	if a.getRepo(viewerLogin, name) != nil {
		writeErr(w, 422, "Repository already exists")
		return
	}
	priv, _ := m["private"].(bool)
	db := str(m, "default_branch")
	if db == "" {
		if v, ok := m["auto_init"].(bool); ok && v {
			db = "main"
		}
	}
	repo := a.createRepo(name, str(m, "description"), priv, db)
	writeJSON(w, 201, a.repoObj(repo))
}

func (a *App) restListUserRepos(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perPage := 30
	if v := q.Get("per_page"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			perPage = n
		}
	}
	page := 1
	if v := q.Get("page"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			page = n
		}
	}
	var all []*Repo
	for _, rp := range a.repos {
		if rp.Owner == viewerLogin {
			all = append(all, rp)
		}
	}
	// stable order by id
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			if all[j].ID < all[i].ID {
				all[i], all[j] = all[j], all[i]
			}
		}
	}
	start := (page - 1) * perPage
	end := start + perPage
	if start > len(all) {
		start = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	var out []map[string]any
	for _, rp := range all[start:end] {
		out = append(out, a.repoObj(rp))
	}
	if out == nil {
		out = []map[string]any{}
	}
	if end < len(all) {
		nextPage := page + 1
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
			scheme = fp
		}
		w.Header().Set("Link", fmt.Sprintf("<%s://%s/api/v3/user/repos?per_page=%d&page=%d>; rel=\"next\"", scheme, r.Host, perPage, nextPage))
	}
	writeJSON(w, 200, out)
}

func (a *App) serveRepoRoutes(w http.ResponseWriter, r *http.Request, owner, name string, rest []string) {
	repo := a.getRepo(owner, name)
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	method := r.Method
	sub := strings.Join(rest, "/")

	if len(rest) == 0 {
		switch method {
		case "GET":
			writeJSON(w, 200, a.repoObj(repo))
		case "PATCH":
			m, err := parseJSONBody(r)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			if v, ok := m["description"]; ok {
				if s, ok := v.(string); ok {
					repo.Description = s
				}
			}
			if v, ok := m["default_branch"].(string); ok {
				repo.DefaultBranch = v
			}
			if v, ok := m["private"].(bool); ok {
				repo.Private = v
			}
			writeJSON(w, 200, a.repoObj(repo))
		case "DELETE":
			delete(a.repos, owner+"/"+name)
			w.WriteHeader(204)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	switch {
	case sub == "branches" && method == "GET":
		a.restBranches(w, repo)
	case strings.HasPrefix(sub, "branches/") && method == "GET":
		a.restBranchOne(w, repo, strings.TrimPrefix(sub, "branches/"))
	case sub == "commits" && method == "GET":
		a.restCommits(w, r, repo)
	case strings.HasPrefix(sub, "commits/") && method == "GET":
		a.restCommitOne(w, repo, strings.TrimPrefix(sub, "commits/"))
	case sub == "git/refs" && method == "POST":
		a.restCreateRef(w, r, repo)
	case strings.HasPrefix(sub, "git/refs/") && method == "GET":
		a.restGetRef(w, repo, "refs/"+strings.TrimPrefix(sub, "git/refs/"))
	case strings.HasPrefix(sub, "git/commits/") && method == "GET":
		a.restGitCommit(w, repo, strings.TrimPrefix(sub, "git/commits/"))
	case strings.HasPrefix(sub, "contents/"):
		a.restContents(w, r, repo, strings.TrimPrefix(sub, "contents/"))
	case sub == "labels" && method == "POST":
		a.restCreateLabel(w, r, repo)
	case sub == "labels" && method == "GET":
		a.restListLabels(w, repo)
	case sub == "issues" && method == "POST":
		a.restCreateIssue(w, r, repo)
	case sub == "issues" && method == "GET":
		a.restListIssues(w, r, repo)
	case strings.HasPrefix(sub, "issues/"):
		a.serveIssueRoutes(w, r, repo, strings.TrimPrefix(sub, "issues/"))
	case sub == "pulls" && method == "POST":
		a.restCreatePull(w, r, repo)
	case sub == "pulls" && method == "GET":
		a.restListPulls(w, r, repo)
	case strings.HasPrefix(sub, "pulls/"):
		a.servePullRoutes(w, r, repo, strings.TrimPrefix(sub, "pulls/"))
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (a *App) restBranches(w http.ResponseWriter, repo *Repo) {
	var out []map[string]any
	for ref, sha := range repo.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			bn := strings.TrimPrefix(ref, "refs/heads/")
			out = append(out, map[string]any{
				"name": bn, "commit": a.commitObj(repo, sha), "protected": false,
			})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (a *App) restBranchOne(w http.ResponseWriter, repo *Repo, name string) {
	sha := repo.tip("refs/heads/" + name)
	if sha == "" {
		writeErr(w, 404, "Branch not found")
		return
	}
	writeJSON(w, 200, map[string]any{"name": name, "commit": a.commitObj(repo, sha), "protected": false})
}

func (a *App) commitObj(repo *Repo, sha string) map[string]any {
	o := repo.getObj(sha)
	msg := ""
	var author, committer map[string]any
	if o != nil && o.Type == "commit" {
		c := parseCommit(o.Data)
		msg = c.Message
		author = identObj(c.Author)
		committer = identObj(c.Committer)
	}
	return map[string]any{
		"sha": sha, "node_id": "C_" + sha, "commit": map[string]any{
			"message": msg, "author": author, "committer": committer,
			"tree": map[string]any{"sha": treeOf(repo, sha)},
		},
		"author": a.userObj(viewerLogin), "committer": a.userObj(viewerLogin),
		"parents": parentsOf(repo, sha),
	}
}

func treeOf(repo *Repo, sha string) string {
	o := repo.getObj(sha)
	if o != nil && o.Type == "commit" {
		return parseCommit(o.Data).Tree
	}
	return ""
}

func parentsOf(repo *Repo, sha string) []map[string]any {
	o := repo.getObj(sha)
	var out []map[string]any
	if o != nil && o.Type == "commit" {
		for _, p := range parseCommit(o.Data).Parents {
			out = append(out, map[string]any{"sha": p})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func identObj(line string) map[string]any {
	ep := parseIdentEpoch(line)
	return map[string]any{
		"name": identName(line), "email": identEmail(line),
		"date": epochISO(ep),
	}
}

func epochISO(ep int64) string {
	return fmt.Sprintf("%s", timeUnix(ep))
}

func (a *App) restCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ref := r.URL.Query().Get("sha")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	full := ref
	if !strings.HasPrefix(ref, "refs/") {
		full = "refs/heads/" + ref
	}
	sha := repo.tip(full)
	if sha == "" {
		sha = repo.tip("refs/heads/" + ref)
	}
	var out []map[string]any
	seen := map[string]bool{}
	for sha != "" && !seen[sha] {
		seen[sha] = true
		out = append(out, a.commitObj(repo, sha))
		o := repo.getObj(sha)
		if o == nil || o.Type != "commit" {
			break
		}
		ps := parseCommit(o.Data).Parents
		if len(ps) == 0 {
			break
		}
		sha = ps[0]
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (a *App) restCommitOne(w http.ResponseWriter, repo *Repo, sha string) {
	if repo.getObj(sha) == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, a.commitObj(repo, sha))
}

func (a *App) restGitCommit(w http.ResponseWriter, repo *Repo, sha string) {
	o := repo.getObj(sha)
	if o == nil || o.Type != "commit" {
		writeErr(w, 404, "Not Found")
		return
	}
	c := parseCommit(o.Data)
	writeJSON(w, 200, map[string]any{
		"sha": sha, "tree": map[string]any{"sha": c.Tree},
		"message": c.Message, "author": identObj(c.Author), "committer": identObj(c.Committer),
		"parents": parentsOf(repo, sha),
	})
}

func (a *App) restCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m, err := parseJSONBody(r)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	ref := str(m, "ref")
	sha := str(m, "sha")
	if !strings.HasPrefix(ref, "refs/") {
		ref = "refs/heads/" + ref
	}
	if repo.getObj(sha) == nil {
		writeErr(w, 422, "Object does not exist")
		return
	}
	repo.Refs[ref] = sha
	writeJSON(w, 201, map[string]any{
		"ref": ref, "node_id": "REF_" + ref, "url": "https://forge.local/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/" + ref,
		"object": map[string]any{"sha": sha, "type": "commit", "url": "https://forge.local/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/commits/" + sha},
	})
}

func (a *App) restGetRef(w http.ResponseWriter, repo *Repo, ref string) {
	sha := repo.tip(ref)
	if sha == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ref": ref, "object": map[string]any{"sha": sha, "type": "commit"},
	})
}

func (a *App) restCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m, err := parseJSONBody(r)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	name := str(m, "name")
	color := str(m, "color")
	if color == "" {
		color = "ededed"
	}
	color = strings.TrimPrefix(color, "#")
	l := &Label{Name: name, Color: color}
	repo.Labels[name] = l
	writeJSON(w, 201, map[string]any{
		"id": a.nextID(), "node_id": "L_" + name, "name": name, "color": color,
		"default": false, "description": str(m, "description"),
	})
}

func (a *App) restListLabels(w http.ResponseWriter, repo *Repo) {
	var out []map[string]any
	for _, l := range repo.Labels {
		out = append(out, map[string]any{"name": l.Name, "color": l.Color, "default": false})
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (a *App) labelObj(name string, repo *Repo) map[string]any {
	color := "ededed"
	if l := repo.Labels[name]; l != nil {
		color = l.Color
	}
	return map[string]any{"id": a.nextID(), "name": name, "color": color, "default": false}
}

func (a *App) issueObj(repo *Repo, it *Issue) map[string]any {
	var labels []map[string]any
	for _, ln := range it.Labels {
		labels = append(labels, a.labelObj(ln, repo))
	}
	if labels == nil {
		labels = []map[string]any{}
	}
	return map[string]any{
		"id": 100000 + it.Number, "node_id": "I_" + strconv.Itoa(it.Number),
		"number": it.Number, "title": it.Title, "body": it.Body, "state": it.State,
		"labels": labels, "user": a.userObj(viewerLogin), "assignees": []any{},
		"comments": len(it.Comments), "created_at": it.CreatedAt, "updated_at": it.UpdatedAt,
		"closed_at": nil, "locked": false,
		"html_url": fmt.Sprintf("https://forge.local/%s/%s/issues/%d", repo.Owner, repo.Name, it.Number),
	}
}

func (a *App) restCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m, err := parseJSONBody(r)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	title := str(m, "title")
	if title == "" {
		writeErr(w, 422, "Title is required")
		return
	}
	var labels []string
	if arr, ok := m["labels"].([]any); ok {
		for _, v := range arr {
			ln, _ := v.(string)
			if repo.Labels[ln] == nil {
				writeErr(w, 422, "Label does not exist: "+ln)
				return
			}
			labels = append(labels, ln)
		}
	}
	repo.issueSeq++
	_, ts := a.nextTime()
	it := &Issue{Number: repo.issueSeq, Title: title, Body: str(m, "body"), State: "open", Labels: labels, CreatedAt: ts, UpdatedAt: ts}
	repo.Issues = append(repo.Issues, it)
	writeJSON(w, 201, a.issueObj(repo, it))
}

func (a *App) findIssue(repo *Repo, n int) *Issue {
	for _, it := range repo.Issues {
		if it.Number == n {
			return it
		}
	}
	return nil
}

func (a *App) restListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortKey := q.Get("sort")
	dir := q.Get("direction")
	var list []*Issue
	for _, it := range repo.Issues {
		if state == "all" || it.State == state {
			list = append(list, it)
		}
	}
	// sort by created (number order is creation order)
	if sortKey == "created" || sortKey == "" {
		if dir == "desc" {
			for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
				list[i], list[j] = list[j], list[i]
			}
		}
	}
	var out []map[string]any
	for _, it := range list {
		out = append(out, a.issueObj(repo, it))
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (a *App) serveIssueRoutes(w http.ResponseWriter, r *http.Request, repo *Repo, sub string) {
	parts := strings.SplitN(sub, "/", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	it := a.findIssue(repo, n)
	if it == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	method := r.Method
	if len(parts) == 1 {
		switch method {
		case "GET":
			writeJSON(w, 200, a.issueObj(repo, it))
		case "PATCH":
			m, err := parseJSONBody(r)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			if v, ok := m["title"].(string); ok {
				it.Title = v
			}
			if v, ok := m["body"].(string); ok {
				it.Body = v
			}
			if v, ok := m["state"].(string); ok {
				it.State = v
			}
			_, it.UpdatedAt = a.nextTime()
			writeJSON(w, 200, a.issueObj(repo, it))
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	if parts[1] == "comments" {
		if method == "POST" {
			m, err := parseJSONBody(r)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			repo.commentSeq++
			_, ts := a.nextTime()
			c := &Comment{ID: repo.commentSeq, Body: str(m, "body"), CreatedAt: ts}
			it.Comments = append(it.Comments, c)
			writeJSON(w, 201, a.commentObj(c))
			return
		}
		if method == "GET" {
			var out []map[string]any
			for _, c := range it.Comments {
				out = append(out, a.commentObj(c))
			}
			if out == nil {
				out = []map[string]any{}
			}
			writeJSON(w, 200, out)
			return
		}
	}
	writeErr(w, 404, "Not Found")
}

func (a *App) commentObj(c *Comment) map[string]any {
	return map[string]any{
		"id": c.ID, "node_id": "IC_" + strconv.Itoa(c.ID), "body": c.Body,
		"user": a.userObj(viewerLogin), "created_at": c.CreatedAt, "updated_at": c.CreatedAt,
	}
}

func (a *App) pullObj(repo *Repo, p *Pull) map[string]any {
	return map[string]any{
		"id": 200000 + p.Number, "node_id": "PR_" + strconv.Itoa(p.Number),
		"number": p.Number, "state": p.State, "title": p.Title, "body": p.Body,
		"locked": false, "user": a.userObj(viewerLogin),
		"head": map[string]any{"ref": p.HeadRef, "sha": p.HeadSHA, "label": repo.Owner + ":" + p.HeadRef, "user": a.userObj(viewerLogin), "repo": a.repoObj(repo)},
		"base": map[string]any{"ref": p.BaseRef, "sha": p.BaseSHA, "label": repo.Owner + ":" + p.BaseRef, "user": a.userObj(viewerLogin), "repo": a.repoObj(repo)},
		"merged": p.Merged, "merge_commit_sha": p.MergeCommitSHA,
		"created_at": p.CreatedAt, "updated_at": p.UpdatedAt, "closed_at": closedAt(p), "merged_at": p.MergedAt,
		"mergeable": true, "draft": false,
		"html_url": fmt.Sprintf("https://forge.local/%s/%s/pull/%d", repo.Owner, repo.Name, p.Number),
	}
}

func closedAt(p *Pull) any {
	if p.State == "closed" {
		if p.MergedAt != "" {
			return p.MergedAt
		}
		return p.UpdatedAt
	}
	return nil
}

func (a *App) restCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	m, err := parseJSONBody(r)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	head := str(m, "head")
	base := str(m, "base")
	if base == "" {
		base = repo.DefaultBranch
	}
	headSHA := repo.tip("refs/heads/" + head)
	baseSHA := repo.tip("refs/heads/" + base)
	if headSHA == "" {
		writeErr(w, 422, "Head branch not found")
		return
	}
	repo.pullSeq++
	_, ts := a.nextTime()
	p := &Pull{Number: repo.pullSeq, Title: str(m, "title"), Body: str(m, "body"), State: "open", HeadRef: head, HeadSHA: headSHA, BaseRef: base, BaseSHA: baseSHA, CreatedAt: ts, UpdatedAt: ts}
	repo.Pulls = append(repo.Pulls, p)
	writeJSON(w, 201, a.pullObj(repo, p))
}

func (a *App) findPull(repo *Repo, n int) *Pull {
	for _, p := range repo.Pulls {
		if p.Number == n {
			return p
		}
	}
	return nil
}

func (a *App) restListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	state := r.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	var out []map[string]any
	for _, p := range repo.Pulls {
		if state == "all" || p.State == state {
			out = append(out, a.pullObj(repo, p))
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, 200, out)
}

func (a *App) servePullRoutes(w http.ResponseWriter, r *http.Request, repo *Repo, sub string) {
	parts := strings.SplitN(sub, "/", 2)
	n, err := strconv.Atoi(parts[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	p := a.findPull(repo, n)
	method := r.Method
	if len(parts) == 1 {
		if p == nil {
			writeErr(w, 404, "Not Found")
			return
		}
		if method == "GET" {
			writeJSON(w, 200, a.pullObj(repo, p))
			return
		}
		if method == "PATCH" {
			m, _ := parseJSONBody(r)
			if v, ok := m["state"].(string); ok {
				p.State = v
			}
			writeJSON(w, 200, a.pullObj(repo, p))
			return
		}
		writeErr(w, 404, "Not Found")
		return
	}
	if p == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	switch {
	case parts[1] == "files" && method == "GET":
		a.restPullFiles(w, repo, p)
	case parts[1] == "merge" && method == "PUT":
		a.restMergePull(w, r, repo, p)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (a *App) pullFiles(repo *Repo, p *Pull) []map[string]any {
	baseFiles := repo.filesAt("refs/heads/" + p.BaseRef)
	headFiles := repo.filesAt("refs/heads/" + p.HeadRef)
	var out []map[string]any
	for path, sha := range headFiles {
		bsha, exists := baseFiles[path]
		status := "added"
		additions, deletions := 0, 0
		if exists {
			if bsha == sha {
				continue
			}
			status = "modified"
		}
		if o := repo.getObj(sha); o != nil {
			additions = len(strings.Split(string(o.Data), "\n"))
		}
		out = append(out, map[string]any{
			"sha": sha, "filename": path, "status": status,
			"additions": additions, "deletions": deletions, "changes": additions + deletions,
			"blob_url": "", "raw_url": "", "contents_url": "",
		})
	}
	for path := range baseFiles {
		if _, ok := headFiles[path]; !ok {
			out = append(out, map[string]any{"filename": path, "status": "removed", "additions": 0, "deletions": 1, "changes": 1})
		}
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}

func (a *App) restPullFiles(w http.ResponseWriter, repo *Repo, p *Pull) {
	writeJSON(w, 200, a.pullFiles(repo, p))
}

func (a *App) restMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, p *Pull) {
	m, _ := parseJSONBody(r)
	method := str(m, "merge_method")
	if method == "" {
		method = "merge"
	}
	if p.Merged {
		writeErr(w, 405, "Pull request already merged")
		return
	}
	baseRef := "refs/heads/" + p.BaseRef
	headRef := "refs/heads/" + p.HeadRef
	baseSHA := repo.tip(baseRef)
	headSHA := repo.tip(headRef)
	// conflict detection: if both branches changed the same path differently since merge base
	if a.conflicted(repo, baseSHA, headSHA) {
		writeErr(w, 405, "Merge conflict")
		return
	}
	headFiles := repo.filesAt(headRef)
	var newSHA string
	_, ts := a.nextTime()
	ep, _ := a.nextTime()
	if method == "squash" {
		tree := repo.buildTreeRecursive(headFiles)
		msg := p.Title + " (#" + strconv.Itoa(p.Number) + ")"
		newSHA = repo.commitTree(tree, baseSHA, msg, ep)
	} else {
		tree := repo.buildTreeRecursive(headFiles)
		id := identity(viewerName, viewerEmail, ep)
		c := commitInfo{Tree: tree, Parents: []string{baseSHA, headSHA}, Author: id, Committer: id, Message: "Merge pull request #" + strconv.Itoa(p.Number)}
		newSHA = repo.putObj("commit", buildCommit(c))
	}
	repo.Refs[baseRef] = newSHA
	p.Merged = true
	p.State = "closed"
	p.MergeCommitSHA = newSHA
	p.MergedAt = ts
	p.UpdatedAt = ts
	writeJSON(w, 200, map[string]any{"sha": newSHA, "merged": true, "message": "Pull Request successfully merged"})
}

func (a *App) conflicted(repo *Repo, baseSHA, headSHA string) bool {
	mb := a.mergeBase(repo, baseSHA, headSHA)
	if mb == "" {
		return false
	}
	mbFiles := repo.filesAtCommit(mb)
	baseFiles := repo.filesAtCommit(baseSHA)
	headFiles := repo.filesAtCommit(headSHA)
	for path, bs := range baseFiles {
		ms, mok := mbFiles[path]
		hs, hok := headFiles[path]
		if !mok || !hok {
			continue
		}
		if bs != ms && hs != ms && bs != hs {
			return true
		}
	}
	return false
}

func (r *Repo) filesAtCommit(sha string) map[string]string {
	out := map[string]string{}
	o := r.getObj(sha)
	if o == nil || o.Type != "commit" {
		return out
	}
	r.flattenTree(parseCommit(o.Data).Tree, "", out)
	return out
}

func (a *App) mergeBase(repo *Repo, x, y string) string {
	anc := map[string]bool{}
	for x != "" {
		anc[x] = true
		o := repo.getObj(x)
		if o == nil || o.Type != "commit" {
			break
		}
		ps := parseCommit(o.Data).Parents
		if len(ps) == 0 {
			break
		}
		x = ps[0]
	}
	for y != "" {
		if anc[y] {
			return y
		}
		o := repo.getObj(y)
		if o == nil || o.Type != "commit" {
			break
		}
		ps := parseCommit(o.Data).Parents
		if len(ps) == 0 {
			break
		}
		y = ps[0]
	}
	return ""
}

func (a *App) restContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	path = strings.TrimSuffix(path, "/")
	method := r.Method
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		ref = repo.DefaultBranch
	}
	fullRef := "refs/heads/" + ref
	switch method {
	case "GET":
		files := repo.filesAt(fullRef)
		if sha, ok := files[path]; ok {
			o := repo.getObj(sha)
			content := ""
			size := 0
			if o != nil {
				content = base64.StdEncoding.EncodeToString(o.Data)
				size = len(o.Data)
			}
			writeJSON(w, 200, map[string]any{
				"name": lastSeg(path), "path": path, "sha": sha, "size": size,
				"encoding": "base64", "content": content, "type": "file",
			})
			return
		}
		// directory listing
		var out []map[string]any
		prefix := path + "/"
		if path == "" {
			prefix = ""
		}
		seen := map[string]bool{}
		for fp, sha := range files {
			if !strings.HasPrefix(fp, prefix) {
				continue
			}
			rest := strings.TrimPrefix(fp, prefix)
			i := strings.IndexByte(rest, '/')
			if i < 0 {
				out = append(out, map[string]any{"name": rest, "path": fp, "sha": sha, "size": objSize(repo, sha), "type": "file"})
			} else {
				d := rest[:i]
				if !seen[d] {
					seen[d] = true
					out = append(out, map[string]any{"name": d, "path": prefix + d, "sha": "", "size": 0, "type": "dir"})
				}
			}
		}
		if len(out) == 0 {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, out)
	case "PUT":
		m, err := parseJSONBody(r)
		if err != nil {
			writeErr(w, 400, "Problems parsing JSON")
			return
		}
		if b := str(m, "branch"); b != "" {
			fullRef = "refs/heads/" + b
		}
		contentB64 := str(m, "content")
		data, derr := base64.StdEncoding.DecodeString(strings.ReplaceAll(contentB64, "\n", ""))
		if derr != nil {
			writeErr(w, 400, "Invalid base64 content")
			return
		}
		files := repo.filesAt(fullRef)
		oldSHA, exists := files[path]
		clientSHA := str(m, "sha")
		if exists && clientSHA != "" && clientSHA != oldSHA {
			writeErr(w, 409, "Conflict: stale sha")
			return
		}
		blobSHA := repo.putObj("blob", data)
		files[path] = blobSHA
		tree := repo.buildTreeRecursive(files)
		parent := repo.tip(fullRef)
		msg := str(m, "message")
		if msg == "" {
			msg = "Update " + path
		}
		ep, _ := a.nextTime()
		commitSHA := repo.commitTree(tree, parent, msg, ep)
		repo.Refs[fullRef] = commitSHA
		status := 201
		if exists {
			status = 200
		}
		writeJSON(w, status, map[string]any{
			"content": map[string]any{"name": lastSeg(path), "path": path, "sha": blobSHA, "size": len(data), "encoding": "base64", "content": contentB64, "type": "file"},
			"commit":  map[string]any{"sha": commitSHA, "message": msg},
		})
	case "DELETE":
		m, err := parseJSONBody(r)
		if err != nil {
			writeErr(w, 400, "Problems parsing JSON")
			return
		}
		if b := str(m, "branch"); b != "" {
			fullRef = "refs/heads/" + b
		}
		files := repo.filesAt(fullRef)
		if _, ok := files[path]; !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		delete(files, path)
		tree := repo.buildTreeRecursive(files)
		parent := repo.tip(fullRef)
		msg := str(m, "message")
		if msg == "" {
			msg = "Delete " + path
		}
		ep, _ := a.nextTime()
		commitSHA := repo.commitTree(tree, parent, msg, ep)
		repo.Refs[fullRef] = commitSHA
		writeJSON(w, 200, map[string]any{"content": nil, "commit": map[string]any{"sha": commitSHA, "message": msg}})
	default:
		writeErr(w, 404, "Not Found")
	}
}

func lastSeg(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func objSize(repo *Repo, sha string) int {
	if o := repo.getObj(sha); o != nil {
		return len(o.Data)
	}
	return 0
}