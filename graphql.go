package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---- global (Relay) ids: opaque, stable, base64 of "Type:keys" ----

func gid(parts ...string) string {
	return base64.StdEncoding.EncodeToString([]byte(strings.Join(parts, ":")))
}
func repoGID(r *Repo) string         { return gid("Repository", strconv.Itoa(r.ID)) }
func issueGID(r *Repo, n int) string { return gid("Issue", strconv.Itoa(r.ID), strconv.Itoa(n)) }
func pullGID(r *Repo, n int) string  { return gid("PullRequest", strconv.Itoa(r.ID), strconv.Itoa(n)) }
func commentGID(id int) string       { return gid("IssueComment", strconv.Itoa(id)) }

func decodeGID(id string) (string, []string) {
	b, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		return "", nil
	}
	parts := strings.Split(string(b), ":")
	return parts[0], parts[1:]
}

func (a *App) repoByID(id int) *Repo {
	for _, r := range a.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

// ---- GraphQL subset parser ----

type gqlValue struct {
	kind string // var,int,float,string,bool,null,enum,list,object
	str  string
	num  float64
	b    bool
	list []gqlValue
	obj  map[string]gqlValue
}

type gqlField struct {
	alias string
	name  string
	args  map[string]gqlValue
	sels  []gqlSelection
}

type gqlSelection struct {
	field        *gqlField
	fragmentType string
	fragSels     []gqlSelection
}

type gqlOp struct {
	kind string
	name string
	sels []gqlSelection
}

type gqlParser struct {
	s   string
	pos int
}

func (p *gqlParser) ws() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' {
			p.pos++
		} else if c == '#' {
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		} else {
			break
		}
	}
}

func (p *gqlParser) peek() byte {
	p.ws()
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
func isIdent(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

func (p *gqlParser) ident() string {
	p.ws()
	start := p.pos
	for p.pos < len(p.s) && isIdent(p.s[p.pos]) {
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *gqlParser) expect(c byte) bool {
	if p.peek() == c {
		p.pos++
		return true
	}
	return false
}

func (p *gqlParser) parseDocument() ([]gqlOp, error) {
	var ops []gqlOp
	for {
		p.ws()
		if p.pos >= len(p.s) {
			break
		}
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("empty document")
	}
	return ops, nil
}

func (p *gqlParser) parseOperation() (gqlOp, error) {
	op := gqlOp{kind: "query"}
	if p.peek() == '{' {
		sels, err := p.parseSelectionSet()
		op.sels = sels
		return op, err
	}
	kw := p.ident()
	if kw == "mutation" || kw == "subscription" {
		op.kind = kw
	}
	if isIdentStart(p.peek()) {
		op.name = p.ident()
	}
	if p.peek() == '(' {
		p.skipParens()
	}
	for p.peek() == '@' {
		p.pos++
		p.ident()
		if p.peek() == '(' {
			p.skipParens()
		}
	}
	sels, err := p.parseSelectionSet()
	op.sels = sels
	return op, err
}

func (p *gqlParser) skipParens() {
	p.expect('(')
	depth := 1
	for p.pos < len(p.s) && depth > 0 {
		c := p.s[p.pos]
		if c == '(' {
			depth++
		} else if c == ')' {
			depth--
		}
		p.pos++
	}
}

func (p *gqlParser) parseSelectionSet() ([]gqlSelection, error) {
	if !p.expect('{') {
		return nil, fmt.Errorf("expected {")
	}
	var sels []gqlSelection
	for {
		if p.peek() == '}' {
			p.pos++
			break
		}
		if p.pos >= len(p.s) {
			return nil, fmt.Errorf("unterminated selection set")
		}
		s, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, s)
	}
	return sels, nil
}

func (p *gqlParser) parseSelection() (gqlSelection, error) {
	if p.peek() == '.' {
		for p.pos < len(p.s) && p.s[p.pos] == '.' {
			p.pos++
		}
		p.ws()
		if p.peek() == '@' {
			p.pos++
			p.ident()
			if p.peek() == '(' {
				p.skipParens()
			}
			return gqlSelection{}, nil
		}
		kw := p.ident()
		if kw == "on" {
			tn := p.ident()
			sels, err := p.parseSelectionSet()
			return gqlSelection{fragmentType: tn, fragSels: sels}, err
		}
		for p.peek() == '@' {
			p.pos++
			p.ident()
			if p.peek() == '(' {
				p.skipParens()
			}
		}
		return gqlSelection{}, nil
	}
	f := &gqlField{args: map[string]gqlValue{}}
	f.name = p.ident()
	if f.name == "" {
		return gqlSelection{}, fmt.Errorf("expected field name")
	}
	if p.peek() == ':' {
		p.pos++
		f.alias = f.name
		f.name = p.ident()
	}
	if p.peek() == '(' {
		args, err := p.parseArgs()
		if err != nil {
			return gqlSelection{}, err
		}
		f.args = args
	}
	for p.peek() == '@' {
		p.pos++
		p.ident()
		if p.peek() == '(' {
			p.skipParens()
		}
	}
	if p.peek() == '{' {
		sels, err := p.parseSelectionSet()
		if err != nil {
			return gqlSelection{}, err
		}
		f.sels = sels
	}
	return gqlSelection{field: f}, nil
}

func (p *gqlParser) parseArgs() (map[string]gqlValue, error) {
	p.expect('(')
	args := map[string]gqlValue{}
	for {
		if p.peek() == ')' {
			p.pos++
			break
		}
		name := p.ident()
		if !p.expect(':') {
			return nil, fmt.Errorf("expected : in args")
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = v
	}
	return args, nil
}

func (p *gqlParser) parseValue() (gqlValue, error) {
	c := p.peek()
	switch {
	case c == '$':
		p.pos++
		return gqlValue{kind: "var", str: p.ident()}, nil
	case c == '"':
		s, err := p.parseString()
		return gqlValue{kind: "string", str: s}, err
	case c == '[':
		p.pos++
		var list []gqlValue
		for {
			if p.peek() == ']' {
				p.pos++
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			list = append(list, v)
		}
		return gqlValue{kind: "list", list: list}, nil
	case c == '{':
		p.pos++
		obj := map[string]gqlValue{}
		for {
			if p.peek() == '}' {
				p.pos++
				break
			}
			name := p.ident()
			if !p.expect(':') {
				return gqlValue{}, fmt.Errorf("expected : in object")
			}
			v, err := p.parseValue()
			if err != nil {
				return gqlValue{}, err
			}
			obj[name] = v
		}
		return gqlValue{kind: "object", obj: obj}, nil
	case c == '-' || (c >= '0' && c <= '9'):
		start := p.pos
		if c == '-' {
			p.pos++
		}
		isFloat := false
		for p.pos < len(p.s) {
			ch := p.s[p.pos]
			if ch >= '0' && ch <= '9' {
				p.pos++
			} else if ch == '.' || ch == 'e' || ch == 'E' || ch == '+' || ch == '-' {
				isFloat = true
				p.pos++
			} else {
				break
			}
		}
		n, _ := strconv.ParseFloat(p.s[start:p.pos], 64)
		k := "int"
		if isFloat {
			k = "float"
		}
		return gqlValue{kind: k, num: n}, nil
	default:
		id := p.ident()
		switch id {
		case "true":
			return gqlValue{kind: "bool", b: true}, nil
		case "false":
			return gqlValue{kind: "bool", b: false}, nil
		case "null":
			return gqlValue{kind: "null"}, nil
		case "":
			return gqlValue{}, fmt.Errorf("unexpected value")
		default:
			return gqlValue{kind: "enum", str: id}, nil
		}
	}
}

func (p *gqlParser) parseString() (string, error) {
	p.expect('"')
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		if c == '"' {
			return b.String(), nil
		}
		if c == '\\' && p.pos < len(p.s) {
			e := p.s[p.pos]
			p.pos++
			switch e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case '/':
				b.WriteByte('/')
			default:
				b.WriteByte(e)
			}
		} else {
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("unterminated string")
}

func resolveValue(v gqlValue, vars map[string]any) any {
	switch v.kind {
	case "var":
		return vars[v.str]
	case "string", "enum":
		return v.str
	case "int", "float":
		return v.num
	case "bool":
		return v.b
	case "null":
		return nil
	case "list":
		out := make([]any, 0, len(v.list))
		for _, e := range v.list {
			out = append(out, resolveValue(e, vars))
		}
		return out
	case "object":
		out := map[string]any{}
		for k, e := range v.obj {
			out[k] = resolveValue(e, vars)
		}
		return out
	}
	return nil
}

func resolveArgs(args map[string]gqlValue, vars map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range args {
		out[k] = resolveValue(v, vars)
	}
	return out
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

// ---- executor ----

type gqlError struct {
	Message string `json:"message"`
}

type resolved struct {
	val  any
	typ  string
	list bool
}

var issueSchema = []string{"id", "number", "title", "state", "body", "bodyText", "createdAt", "updatedAt", "author", "labels", "comments", "closed", "closedAt", "repository", "url"}
var pullSchema = []string{"id", "number", "title", "body", "state", "merged", "mergeable", "headRefName", "baseRefName", "files", "createdAt", "updatedAt", "author", "commits", "reviews", "url", "mergeCommit"}
var repoSchema = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "updatedAt", "url"}
var searchEnum = []string{"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "ORGANIZATION"}

func fieldNames(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, n := range names {
		out = append(out, map[string]any{"name": n})
	}
	return out
}

func (a *App) repoNode(r *Repo) map[string]any {
	return map[string]any{
		"__typename": "Repository", "_repo": r,
		"id": repoGID(r), "databaseId": r.ID, "name": r.Name,
		"nameWithOwner": r.Owner + "/" + r.Name, "viewerPermission": "ADMIN",
		"hasIssuesEnabled": true, "description": r.Description, "isPrivate": r.Private,
	}
}

func (a *App) issueNode(r *Repo, it *Issue) map[string]any {
	return map[string]any{
		"__typename": "Issue", "_repo": r, "_issue": it,
		"id": issueGID(r, it.Number), "number": it.Number, "title": it.Title,
		"body": it.Body, "state": strings.ToUpper(it.State),
	}
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	return strings.ToUpper(p.State)
}

func (a *App) pullNode(r *Repo, p *Pull) map[string]any {
	mergeable := "MERGEABLE"
	if a.conflicted(r, r.tip("refs/heads/"+p.BaseRef), r.tip("refs/heads/"+p.HeadRef)) {
		mergeable = "CONFLICTING"
	}
	return map[string]any{
		"__typename": "PullRequest", "_repo": r, "_pull": p,
		"id": pullGID(r, p.Number), "number": p.Number, "title": p.Title, "body": p.Body,
		"headRefName": p.HeadRef, "baseRefName": p.BaseRef, "state": pullState(p),
		"merged": p.Merged, "mergeable": mergeable,
	}
}

func (a *App) project(typ string, src map[string]any, sels []gqlSelection, vars map[string]any, errs *[]gqlError) map[string]any {
	out := map[string]any{}
	for _, s := range sels {
		if s.field == nil {
			if s.fragmentType == "" || s.fragmentType == typ || s.fragmentType == "Node" {
				sub := a.project(typ, src, s.fragSels, vars, errs)
				for k, v := range sub {
					out[k] = v
				}
			}
			continue
		}
		f := s.field
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		if f.name == "__typename" {
			out[key] = typ
			continue
		}
		args := resolveArgs(f.args, vars)
		rv, ok := a.resolve(typ, f.name, args, src, vars, errs)
		if !ok {
			*errs = append(*errs, gqlError{Message: fmt.Sprintf("Cannot query field '%s' on type '%s'.", f.name, typ)})
			continue
		}
		if len(f.sels) == 0 {
			out[key] = rv.val
			continue
		}
		if rv.list {
			items, _ := rv.val.([]map[string]any)
			arr := make([]map[string]any, 0, len(items))
			for _, it := range items {
				arr = append(arr, a.project(rv.typ, it, f.sels, vars, errs))
			}
			out[key] = arr
		} else {
			if rv.val == nil {
				out[key] = nil
			} else {
				m, _ := rv.val.(map[string]any)
				out[key] = a.project(rv.typ, m, f.sels, vars, errs)
			}
		}
	}
	return out
}

func (a *App) resolve(typ, field string, args map[string]any, src map[string]any, vars map[string]any, errs *[]gqlError) (resolved, bool) {
	switch typ {
	case "Query":
		switch field {
		case "viewer":
			return resolved{val: map[string]any{"__typename": "User", "login": viewerLogin, "name": viewerName, "id": gid("User", viewerLogin)}, typ: "User"}, true
		case "repository":
			owner, _ := args["owner"].(string)
			name, _ := args["name"].(string)
			r := a.getRepo(owner, name)
			if r == nil {
				return resolved{val: nil, typ: "Repository"}, true
			}
			return resolved{val: a.repoNode(r), typ: "Repository"}, true
		case "node":
			id, _ := args["id"].(string)
			return a.resolveNode(id), true
		case "__type":
			name, _ := args["name"].(string)
			return resolved{val: typeNode(name), typ: "__Type"}, true
		case "__schema":
			return resolved{val: map[string]any{"__typename": "__Schema"}, typ: "__Schema"}, true
		}
	case "Mutation":
		return a.resolveMutation(field, args, errs)
	case "Repository":
		r, _ := src["_repo"].(*Repo)
		if r == nil {
			return resolved{}, false
		}
		switch field {
		case "owner":
			return resolved{val: map[string]any{"__typename": "User", "login": r.Owner}, typ: "User"}, true
		case "defaultBranchRef":
			return resolved{val: map[string]any{"__typename": "Ref", "name": r.DefaultBranch}, typ: "Ref"}, true
		case "issue":
			it := a.findIssue(r, toInt(args["number"]))
			if it == nil {
				return resolved{val: nil, typ: "Issue"}, true
			}
			return resolved{val: a.issueNode(r, it), typ: "Issue"}, true
		case "issues":
			return resolved{val: a.issueConnection(r, args), typ: "IssueConnection"}, true
		case "pullRequest":
			p := a.findPull(r, toInt(args["number"]))
			if p == nil {
				return resolved{val: nil, typ: "PullRequest"}, true
			}
			return resolved{val: a.pullNode(r, p), typ: "PullRequest"}, true
		case "pullRequests":
			return resolved{val: a.pullConnection(r, args), typ: "PullRequestConnection"}, true
		default:
			if v, ok := src[field]; ok {
				return resolved{val: v}, true
			}
		}
	case "Issue":
		if v, ok := src[field]; ok {
			return resolved{val: v}, true
		}
	case "PullRequest":
		r, _ := src["_repo"].(*Repo)
		p, _ := src["_pull"].(*Pull)
		if field == "files" && r != nil && p != nil {
			nodes := []map[string]any{}
			for _, f := range a.pullFiles(r, p) {
				nodes = append(nodes, map[string]any{"__typename": "ChangedFile", "path": f["filename"]})
			}
			return resolved{val: map[string]any{"__typename": "ChangedFileConnection", "nodes": nodes}, typ: "ChangedFileConnection"}, true
		}
		if v, ok := src[field]; ok {
			return resolved{val: v}, true
		}
	case "User", "Ref", "ChangedFile", "IssueComment", "PageInfo", "__Field", "__EnumValue":
		if v, ok := src[field]; ok {
			return resolved{val: v}, true
		}
	case "IssueConnection", "PullRequestConnection", "ChangedFileConnection":
		if field == "nodes" {
			elemType := "Issue"
			if typ == "PullRequestConnection" {
				elemType = "PullRequest"
			} else if typ == "ChangedFileConnection" {
				elemType = "ChangedFile"
			}
			nodes, _ := src["nodes"].([]map[string]any)
			return resolved{val: nodes, typ: elemType, list: true}, true
		}
		if v, ok := src[field]; ok {
			if field == "pageInfo" {
				return resolved{val: v, typ: "PageInfo"}, true
			}
			return resolved{val: v}, true
		}
	case "__Type":
		switch field {
		case "fields":
			f, _ := src["fields"].([]map[string]any)
			return resolved{val: f, typ: "__Field", list: true}, true
		case "enumValues":
			e, _ := src["enumValues"].([]map[string]any)
			return resolved{val: e, typ: "__EnumValue", list: true}, true
		default:
			if v, ok := src[field]; ok {
				return resolved{val: v}, true
			}
		}
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			return resolved{val: src["issue"], typ: "Issue"}, true
		}
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			return resolved{val: src["pullRequest"], typ: "PullRequest"}, true
		}
	case "AddCommentPayload":
		if field == "commentEdge" {
			return resolved{val: src["commentEdge"], typ: "CommentEdge"}, true
		}
	case "CommentEdge":
		if field == "node" {
			return resolved{val: src["node"], typ: "IssueComment"}, true
		}
	}
	return resolved{}, false
}

func typeNode(name string) map[string]any {
	n := map[string]any{"__typename": "__Type", "name": name}
	switch name {
	case "Issue":
		n["fields"] = fieldNames(issueSchema)
	case "PullRequest":
		n["fields"] = fieldNames(pullSchema)
	case "Repository":
		n["fields"] = fieldNames(repoSchema)
	case "SearchType":
		n["enumValues"] = fieldNames(searchEnum)
	}
	return n
}

func (a *App) resolveNode(id string) resolved {
	typ, parts := decodeGID(id)
	switch typ {
	case "Repository":
		if len(parts) >= 1 {
			if r := a.repoByID(toInt(parts[0])); r != nil {
				return resolved{val: a.repoNode(r), typ: "Repository"}
			}
		}
	case "Issue":
		if len(parts) >= 2 {
			if r := a.repoByID(toInt(parts[0])); r != nil {
				if it := a.findIssue(r, toInt(parts[1])); it != nil {
					return resolved{val: a.issueNode(r, it), typ: "Issue"}
				}
			}
		}
	case "PullRequest":
		if len(parts) >= 2 {
			if r := a.repoByID(toInt(parts[0])); r != nil {
				if p := a.findPull(r, toInt(parts[1])); p != nil {
					return resolved{val: a.pullNode(r, p), typ: "PullRequest"}
				}
			}
		}
	}
	return resolved{val: nil, typ: "Node"}
}

func (a *App) issueConnection(r *Repo, args map[string]any) map[string]any {
	var list []*Issue
	states, _ := args["states"].([]any)
	for _, it := range r.Issues {
		if len(states) > 0 {
			match := false
			for _, s := range states {
				if strings.EqualFold(it.State, fmt.Sprint(s)) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		list = append(list, it)
	}
	desc := false
	if ob, ok := args["orderBy"].(map[string]any); ok {
		if fmt.Sprint(ob["direction"]) == "DESC" {
			desc = true
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if desc {
			return list[i].Number > list[j].Number
		}
		return list[i].Number < list[j].Number
	})
	offset := 0
	if after, ok := args["after"].(string); ok && after != "" {
		if b, err := base64.StdEncoding.DecodeString(after); err == nil {
			offset, _ = strconv.Atoi(string(b))
		}
	}
	first := toInt(args["first"])
	if first <= 0 {
		first = 30
	}
	if offset > len(list) {
		offset = len(list)
	}
	end := offset + first
	if end > len(list) {
		end = len(list)
	}
	nodes := []map[string]any{}
	for _, it := range list[offset:end] {
		nodes = append(nodes, a.issueNode(r, it))
	}
	hasNext := end < len(list)
	return map[string]any{
		"__typename": "IssueConnection", "nodes": nodes,
		"pageInfo": map[string]any{
			"__typename": "PageInfo", "hasNextPage": hasNext, "hasPreviousPage": offset > 0,
			"endCursor": base64.StdEncoding.EncodeToString([]byte(strconv.Itoa(end))),
		},
	}
}

func (a *App) pullConnection(r *Repo, args map[string]any) map[string]any {
	first := toInt(args["first"])
	if first <= 0 {
		first = 30
	}
	nodes := []map[string]any{}
	for _, p := range r.Pulls {
		if len(nodes) >= first {
			break
		}
		nodes = append(nodes, a.pullNode(r, p))
	}
	return map[string]any{"__typename": "PullRequestConnection", "nodes": nodes,
		"pageInfo": map[string]any{"__typename": "PageInfo", "hasNextPage": false, "endCursor": ""}}
}

func (a *App) resolveMutation(field string, args map[string]any, errs *[]gqlError) (resolved, bool) {
	input, _ := args["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch field {
	case "createIssue":
		r := a.repoByID(toInt(decodeKey(input["repositoryId"])))
		if r == nil {
			*errs = append(*errs, gqlError{Message: "repository not found"})
			return resolved{val: nil, typ: "CreateIssuePayload"}, true
		}
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		r.issueSeq++
		_, ts := a.nextTime()
		it := &Issue{Number: r.issueSeq, Title: title, Body: body, State: "open", CreatedAt: ts, UpdatedAt: ts}
		r.Issues = append(r.Issues, it)
		return resolved{val: map[string]any{"__typename": "CreateIssuePayload", "issue": a.issueNode(r, it)}, typ: "CreateIssuePayload"}, true
	case "closeIssue":
		r, it := a.issueByGID(fmt.Sprint(input["issueId"]))
		if it == nil {
			*errs = append(*errs, gqlError{Message: "issue not found"})
			return resolved{val: nil, typ: "CloseIssuePayload"}, true
		}
		it.State = "closed"
		_, it.UpdatedAt = a.nextTime()
		return resolved{val: map[string]any{"__typename": "CloseIssuePayload", "issue": a.issueNode(r, it)}, typ: "CloseIssuePayload"}, true
	case "createPullRequest":
		r := a.repoByID(toInt(decodeKey(input["repositoryId"])))
		if r == nil {
			*errs = append(*errs, gqlError{Message: "repository not found"})
			return resolved{val: nil, typ: "CreatePullRequestPayload"}, true
		}
		head, _ := input["headRefName"].(string)
		base, _ := input["baseRefName"].(string)
		title, _ := input["title"].(string)
		body, _ := input["body"].(string)
		if base == "" {
			base = r.DefaultBranch
		}
		r.pullSeq++
		_, ts := a.nextTime()
		p := &Pull{Number: r.pullSeq, Title: title, Body: body, State: "open", HeadRef: head, BaseRef: base,
			HeadSHA: r.tip("refs/heads/" + head), BaseSHA: r.tip("refs/heads/" + base), CreatedAt: ts, UpdatedAt: ts}
		r.Pulls = append(r.Pulls, p)
		return resolved{val: map[string]any{"__typename": "CreatePullRequestPayload", "pullRequest": a.pullNode(r, p)}, typ: "CreatePullRequestPayload"}, true
	case "mergePullRequest":
		r, p := a.pullByGID(fmt.Sprint(input["pullRequestId"]))
		if p == nil {
			*errs = append(*errs, gqlError{Message: "pull request not found"})
			return resolved{val: nil, typ: "MergePullRequestPayload"}, true
		}
		method, _ := input["mergeMethod"].(string)
		a.doMerge(r, p, strings.ToLower(method))
		return resolved{val: map[string]any{"__typename": "MergePullRequestPayload", "pullRequest": a.pullNode(r, p)}, typ: "MergePullRequestPayload"}, true
	case "addComment":
		r, it := a.issueByGID(fmt.Sprint(input["subjectId"]))
		if it == nil {
			*errs = append(*errs, gqlError{Message: "subject not found"})
			return resolved{val: nil, typ: "AddCommentPayload"}, true
		}
		body, _ := input["body"].(string)
		r.commentSeq++
		_, ts := a.nextTime()
		c := &Comment{ID: r.commentSeq, Body: body, CreatedAt: ts}
		it.Comments = append(it.Comments, c)
		node := map[string]any{"__typename": "IssueComment", "id": commentGID(c.ID), "body": c.Body}
		edge := map[string]any{"__typename": "CommentEdge", "node": node}
		return resolved{val: map[string]any{"__typename": "AddCommentPayload", "commentEdge": edge}, typ: "AddCommentPayload"}, true
	}
	return resolved{}, false
}

func decodeKey(v any) string {
	s := fmt.Sprint(v)
	_, parts := decodeGID(s)
	if len(parts) >= 1 {
		return parts[0]
	}
	return ""
}

func (a *App) issueByGID(id string) (*Repo, *Issue) {
	typ, parts := decodeGID(id)
	if typ != "Issue" || len(parts) < 2 {
		return nil, nil
	}
	r := a.repoByID(toInt(parts[0]))
	if r == nil {
		return nil, nil
	}
	return r, a.findIssue(r, toInt(parts[1]))
}

func (a *App) pullByGID(id string) (*Repo, *Pull) {
	typ, parts := decodeGID(id)
	if typ != "PullRequest" || len(parts) < 2 {
		return nil, nil
	}
	r := a.repoByID(toInt(parts[0]))
	if r == nil {
		return nil, nil
	}
	return r, a.findPull(r, toInt(parts[1]))
}

// doMerge mirrors the REST squash/merge path so both surfaces agree.
func (a *App) doMerge(r *Repo, p *Pull, method string) {
	if p.Merged {
		return
	}
	baseRef := "refs/heads/" + p.BaseRef
	headRef := "refs/heads/" + p.HeadRef
	baseSHA := r.tip(baseRef)
	headFiles := r.filesAt(headRef)
	ep, ts := a.nextTime()
	var newSHA string
	if method == "squash" || method == "" {
		tree := r.buildTreeRecursive(headFiles)
		newSHA = r.commitTree(tree, baseSHA, p.Title+" (#"+strconv.Itoa(p.Number)+")", ep)
	} else {
		tree := r.buildTreeRecursive(headFiles)
		id := identity(viewerName, viewerEmail, ep)
		c := commitInfo{Tree: tree, Parents: []string{baseSHA, r.tip(headRef)}, Author: id, Committer: id, Message: "Merge pull request #" + strconv.Itoa(p.Number)}
		newSHA = r.putObj("commit", buildCommit(c))
	}
	r.Refs[baseRef] = newSHA
	p.Merged = true
	p.State = "closed"
	p.MergeCommitSHA = newSHA
	p.MergedAt = ts
	p.UpdatedAt = ts
}

// ---- HTTP entry ----

func (a *App) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeJSON(w, 401, map[string]any{"data": nil, "errors": []gqlError{{Message: "Bad credentials"}}})
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		writeJSON(w, 200, map[string]any{"data": nil, "errors": []gqlError{{Message: "invalid JSON body"}}})
		return
	}
	if arr, ok := body.([]any); ok {
		out := make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			m, _ := e.(map[string]any)
			out = append(out, a.execEnvelope(m))
		}
		writeJSON(w, 200, out)
		return
	}
	m, _ := body.(map[string]any)
	writeJSON(w, 200, a.execEnvelope(m))
}

func (a *App) execEnvelope(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{"data": nil, "errors": []gqlError{{Message: "missing operation"}}}
	}
	query, _ := m["query"].(string)
	vars, _ := m["variables"].(map[string]any)
	if vars == nil {
		vars = map[string]any{}
	}
	opName, _ := m["operationName"].(string)
	data, errs := a.execute(query, vars, opName)
	env := map[string]any{"data": data}
	if len(errs) > 0 {
		env["errors"] = errs
	}
	return env
}

func (a *App) execute(query string, vars map[string]any, opName string) (map[string]any, []gqlError) {
	var errs []gqlError
	p := &gqlParser{s: query}
	ops, err := p.parseDocument()
	if err != nil {
		return nil, []gqlError{{Message: "syntax error: " + err.Error()}}
	}
	var op *gqlOp
	for i := range ops {
		if opName == "" || ops[i].name == opName {
			op = &ops[i]
			break
		}
	}
	if op == nil {
		op = &ops[0]
	}
	rootType := "Query"
	if op.kind == "mutation" {
		rootType = "Mutation"
	}
	data := a.project(rootType, map[string]any{}, op.sels, vars, &errs)
	return data, errs
}