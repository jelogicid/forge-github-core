package main

import (
	"os"
	"strings"
	"sync"
	"time"
)

const viewerLogin = "adaiversen"
const viewerName = "Ada Iversen"
const viewerEmail = "adaiversen@example.invalid"
const baseEpoch = int64(1700000000)

type App struct {
	mu    sync.RWMutex
	repos map[string]*Repo
	token string
	clock int64

	// fetchCache memoizes whole fetch responses. A fetch response is a pure
	// function of its args over immutable, content-addressed objects, so the
	// fifty concurrent shallow clones of one tip share a single computed pack.
	fetchCache  sync.Map
	fetchCacheN int64
	idSeq int
}

type Repo struct {
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	ID            int
	CreatedAt     string
	Objects       map[string]*Obj
	Refs          map[string]string // full ref -> oid
	Issues        []*Issue
	Pulls         []*Pull
	Labels        map[string]*Label
	issueSeq      int
	pullSeq       int
	commentSeq    int
}

type Issue struct {
	Number    int
	Title     string
	Body      string
	State     string
	Labels    []string
	Comments  []*Comment
	CreatedAt string
	UpdatedAt string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
}

type Pull struct {
	Number         int
	Title          string
	Body           string
	State          string
	HeadRef        string
	HeadSHA        string
	BaseRef        string
	BaseSHA        string
	Merged         bool
	MergeCommitSHA string
	MergedAt       string
	CreatedAt      string
	UpdatedAt      string
}

type Label struct {
	Name  string
	Color string
}

func NewApp() *App {
	return &App{
		repos: make(map[string]*Repo),
		token: getenvDefault("MAKEFUN_SHOP_TOKEN", "local-token"),
		clock: baseEpoch,
		idSeq: 1000,
	}
}

func getenvDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func (a *App) nextTime() (int64, string) {
	a.clock++
	return a.clock, time.Unix(a.clock, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func timeUnix(ep int64) string {
	return time.Unix(ep, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func (a *App) nextID() int {
	a.idSeq++
	return a.idSeq
}

func (a *App) createRepo(name, desc string, private bool, defaultBranch string) *Repo {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	_, ts := a.nextTime()
	r := &Repo{
		Owner:         viewerLogin,
		Name:          name,
		Description:   desc,
		Private:       private,
		DefaultBranch: defaultBranch,
		ID:            a.nextID(),
		CreatedAt:     ts,
		Objects:       make(map[string]*Obj),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
	}
	a.repos[viewerLogin+"/"+name] = r
	return r
}

func (a *App) getRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return a.repos[viewerLogin+"/"+name]
}

func (r *Repo) putObj(typ string, data []byte) string {
	sha := hashObj(typ, data)
	r.Objects[sha] = &Obj{Type: typ, Data: data}
	return sha
}

func (r *Repo) getObj(sha string) *Obj {
	return r.Objects[sha]
}

func (r *Repo) tip(ref string) string {
	return r.Refs[ref]
}

func (r *Repo) defaultRef() string {
	return "refs/heads/" + r.DefaultBranch
}

// commitTree builds a new commit on top of parent (may be "") with the given tree.
func (r *Repo) commitTree(treeSHA, parent, message string, epoch int64) string {
	id := identity(viewerName, viewerEmail, epoch)
	c := commitInfo{Tree: treeSHA, Author: id, Committer: id, Message: message}
	if parent != "" {
		c.Parents = []string{parent}
	}
	return r.putObj("commit", buildCommit(c))
}

// buildTreeRecursive builds nested trees from a flat path->sha map.
func (r *Repo) buildTreeRecursive(files map[string]string) string {
	// collect immediate children
	blobs := map[string]string{}
	subdirs := map[string]map[string]string{}
	for path, sha := range files {
		i := strings.IndexByte(path, '/')
		if i < 0 {
			blobs[path] = sha
		} else {
			d := path[:i]
			rest := path[i+1:]
			if subdirs[d] == nil {
				subdirs[d] = map[string]string{}
			}
			subdirs[d][rest] = sha
		}
	}
	var entries []treeEntry
	for name, sha := range blobs {
		entries = append(entries, treeEntry{Mode: "100644", Name: name, OID: sha})
	}
	for name, sub := range subdirs {
		sha := r.buildTreeRecursive(sub)
		entries = append(entries, treeEntry{Mode: "40000", Name: name, OID: sha})
	}
	return r.putObj("tree", buildTree(entries))
}

// flattenTree reads a commit's tree into a flat path->blobSHA map.
func (r *Repo) flattenTree(treeSHA, prefix string, out map[string]string) {
	o := r.getObj(treeSHA)
	if o == nil || o.Type != "tree" {
		return
	}
	for _, e := range parseTree(o.Data) {
		p := e.Name
		if prefix != "" {
			p = prefix + "/" + e.Name
		}
		if e.Mode == "40000" {
			r.flattenTree(e.OID, p, out)
		} else {
			out[p] = e.OID
		}
	}
}

func (r *Repo) filesAt(ref string) map[string]string {
	out := map[string]string{}
	sha := r.tip(ref)
	if sha == "" {
		return out
	}
	o := r.getObj(sha)
	if o == nil || o.Type != "commit" {
		return out
	}
	c := parseCommit(o.Data)
	r.flattenTree(c.Tree, "", out)
	return out
}