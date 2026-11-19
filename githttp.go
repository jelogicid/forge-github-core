package main

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

const gitAgent = "agent=makefun-forge/1.0"

func (a *App) serveGit(w http.ResponseWriter, r *http.Request) {
	if !a.authed(r) {
		writeErr(w, 401, "Bad credentials")
		return
	}
	path := r.URL.Path
	var owner, repoName, tail string
	if i := strings.Index(path, ".git/"); i >= 0 {
		prefix := path[1:i]
		ps := strings.SplitN(prefix, "/", 2)
		if len(ps) != 2 {
			writeErr(w, 404, "Not Found")
			return
		}
		owner, repoName = ps[0], ps[1]
		tail = path[i+5:]
	} else if strings.HasSuffix(path, ".git") {
		prefix := strings.TrimSuffix(path[1:], ".git")
		ps := strings.SplitN(prefix, "/", 2)
		if len(ps) != 2 {
			writeErr(w, 404, "Not Found")
			return
		}
		owner, repoName = ps[0], ps[1]
		tail = ""
	} else {
		writeErr(w, 404, "Not Found")
		return
	}
	repo := a.getRepo(owner, repoName)
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}
	switch {
	case tail == "info/refs" && r.Method == "GET":
		a.gitInfoRefs(w, r, repo)
	case tail == "git-upload-pack" && r.Method == "POST":
		a.gitUploadPack(w, r, repo)
	case tail == "git-receive-pack" && r.Method == "POST":
		a.gitReceivePack(w, r, repo)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func gitCaps(repo *Repo, service string) []string {
	if service == "git-receive-pack" {
		return []string{"report-status", "report-status-v2", "side-band-64k", "delete-refs", "ofs-delta", gitAgent}
	}
	caps := []string{"multi_ack", "multi_ack_detailed", "side-band-64k", "ofs-delta", "shallow", "no-progress", "include-tag", "thin-pack", gitAgent}
	if repo.tip(repo.defaultRef()) != "" {
		caps = append(caps, "symref=HEAD:"+repo.defaultRef())
	}
	return caps
}

func (a *App) gitInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo) {
	service := r.URL.Query().Get("service")
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeErr(w, 400, "bad service")
		return
	}
	var body []byte
	body = append(body, pktText("# service="+service+"\n")...)
	body = append(body, pktFlush...)
	v2 := service == "git-upload-pack" && strings.Contains(r.Header.Get("Git-Protocol"), "version=2")
	if v2 {
		body = append(body, pktText("version 2\n")...)
		body = append(body, pktText(gitAgent+"\n")...)
		body = append(body, pktText("ls-refs=unborn\n")...)
		body = append(body, pktText("fetch=shallow filter\n")...)
		body = append(body, pktText("object-format=sha1\n")...)
		body = append(body, pktFlush...)
	} else {
		caps := strings.Join(gitCaps(repo, service), " ")
		refs := a.refList(repo)
		if len(refs) == 0 {
			body = append(body, pktText(zeroOID+" capabilities^{}\x00"+caps+"\n")...)
		} else {
			first := true
			for _, rl := range refs {
				line := rl.oid + " " + rl.name
				if first {
					line += "\x00" + caps
					first = false
				}
				body = append(body, pktText(line+"\n")...)
				if rl.peeled != "" {
					body = append(body, pktText(rl.peeled+" "+rl.name+"^{}\n")...)
				}
			}
		}
		body = append(body, pktFlush...)
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	w.Write(body)
}

type refLine struct {
	name   string
	oid    string
	peeled string
}

// refList returns HEAD (if resolvable) followed by sorted refs.
func (a *App) refList(repo *Repo) []refLine {
	var out []refLine
	headOID := repo.tip(repo.defaultRef())
	if headOID != "" {
		out = append(out, refLine{name: "HEAD", oid: headOID})
	}
	var names []string
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		oid := repo.Refs[name]
		rl := refLine{name: name, oid: oid}
		o := repo.getObj(oid)
		if o != nil && o.Type == "tag" {
			t := parseTagObj(o.Data)
			rl.peeled = t.Object
		}
		out = append(out, rl)
	}
	return out
}

func (a *App) gitUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	raw, _ := io.ReadAll(r.Body)
	packets, _, err := decodePktLines(raw)
	if err != nil {
		writeErr(w, 400, "bad pkt-line")
		return
	}
	var command string
	var args []string
	inArgs := false
	for _, p := range packets {
		if p.kind == "flush" {
			break
		}
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		s := string(p.payload)
		if !inArgs && strings.HasPrefix(s, "command=") {
			command = strings.TrimSuffix(strings.TrimPrefix(s, "command="), "\n")
			continue
		}
		if inArgs {
			args = append(args, strings.TrimSuffix(s, "\n"))
		}
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	var out []byte
	switch command {
	case "ls-refs":
		out = a.lsRefs(repo, args)
	case "fetch":
		out = a.fetchPack(repo, args)
	default:
		out = append(out, pktFlush...)
	}
	w.WriteHeader(200)
	w.Write(out)
}

func (a *App) lsRefs(repo *Repo, args []string) []byte {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, arg := range args {
		switch {
		case arg == "peel":
			peel = true
		case arg == "symrefs":
			symrefs = true
		case arg == "unborn":
			unborn = true
		case strings.HasPrefix(arg, "ref-prefix "):
			prefixes = append(prefixes, strings.TrimPrefix(arg, "ref-prefix "))
		}
	}
	match := func(name string) bool {
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
	var out []byte
	headRef := repo.defaultRef()
	headOID := repo.tip(headRef)
	if match("HEAD") || len(prefixes) == 0 {
		if headOID != "" {
			line := headOID + " HEAD"
			if symrefs {
				line += " symref-target:" + headRef
			}
			out = append(out, pktText(line+"\n")...)
		} else if unborn {
			out = append(out, pktText("unborn HEAD symref-target:"+headRef+"\n")...)
		}
	}
	var names []string
	for name := range repo.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !match(name) {
			continue
		}
		oid := repo.Refs[name]
		line := oid + " " + name
		o := repo.getObj(oid)
		if peel && o != nil && o.Type == "tag" {
			t := parseTagObj(o.Data)
			line += " peeled:" + t.Object
		}
		out = append(out, pktText(line+"\n")...)
	}
	out = append(out, pktFlush...)
	return out
}

func (a *App) fetchPack(repo *Repo, args []string) []byte {
	cacheKey := repo.Owner + "/" + repo.Name + "\x00" + strings.Join(args, "\x00")
	if v, ok := a.fetchCache.Load(cacheKey); ok {
		return v.([]byte)
	}
	var wants, haves, shallows []string
	depth := 0
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "want "):
			wants = append(wants, strings.TrimPrefix(arg, "want "))
		case strings.HasPrefix(arg, "have "):
			haves = append(haves, strings.TrimPrefix(arg, "have "))
		case strings.HasPrefix(arg, "shallow "):
			shallows = append(shallows, strings.TrimPrefix(arg, "shallow "))
		case strings.HasPrefix(arg, "deepen "):
			fmt.Sscanf(strings.TrimPrefix(arg, "deepen "), "%d", &depth)
		}
	}
	objs, newShallow, unshallow := a.selectFetchObjects(repo, wants, haves, shallows, depth)

	var out []byte
	// acknowledgments section
	out = append(out, pktText("acknowledgments\n")...)
	out = append(out, pktText("NAK\n")...)
	out = append(out, pktFlush...)
	// shallow-info section
	if depth > 0 || len(shallows) > 0 {
		out = append(out, pktText("shallow-info\n")...)
		for _, oid := range unshallow {
			out = append(out, pktText("unshallow "+oid+"\n")...)
		}
		for _, oid := range newShallow {
			out = append(out, pktText("shallow "+oid+"\n")...)
		}
		out = append(out, pktFlush...)
	}
	// packfile section (sideband band 1)
	pack := writePackFull(objs)
	out = append(out, pktText("packfile\n")...)
	out = append(out, sidebandChunk(pack, 1)...)
	out = append(out, pktFlush...)
	if atomic.AddInt64(&a.fetchCacheN, 1) > 8192 {
		a.fetchCache.Range(func(k, _ any) bool { a.fetchCache.Delete(k); return true })
		atomic.StoreInt64(&a.fetchCacheN, 0)
	}
	a.fetchCache.Store(cacheKey, out)
	return out
}

// sidebandChunk wraps raw bytes into band-prefixed pkt-lines.
func sidebandChunk(data []byte, band byte) []byte {
	var out []byte
	const maxPayload = 65515
	for len(data) > 0 {
		n := maxPayload
		if n > len(data) {
			n = len(data)
		}
		payload := append([]byte{band}, data[:n]...)
		out = append(out, pktLine(payload)...)
		data = data[n:]
	}
	return out
}

// selectFetchObjects computes the objects to send for a fetch, honoring haves, shallow
// boundaries and depth. Returns objects, new shallow boundaries, and unshallowed commits.
func (a *App) selectFetchObjects(repo *Repo, wants, haves, shallows []string, depth int) ([]*Obj, []string, []string) {
	shallowSet := map[string]bool{}
	for _, s := range shallows {
		shallowSet[s] = true
	}
	// held = closure of haves; shallow boundaries stop traversal (commit held, parents not).
	held := map[string]bool{}
	for _, h := range haves {
		stop := map[string]bool{}
		if shallowSet[h] {
			stop[h] = true
		}
		for oid := range repo.reachable([]string{h}, stop) {
			held[oid] = true
		}
	}
	send := map[string]bool{}
	newShallow := map[string]bool{}
	deepened := map[string]bool{}
	visited := map[string]int{} // commit -> depth

	var walkCommit func(oid string, d int)
	walkCommit = func(oid string, d int) {
		if oid == "" || oid == zeroOID {
			return
		}
		o := repo.getObj(oid)
		if o == nil {
			return
		}
		if o.Type == "tag" {
			if !held[oid] {
				send[oid] = true
			}
			t := parseTagObj(o.Data)
			walkCommit(t.Object, d)
			return
		}
		if o.Type != "commit" {
			return
		}
		if prev, ok := visited[oid]; ok && prev <= d {
			return
		}
		visited[oid] = d
		if held[oid] {
			if shallowSet[oid] && (depth == 0 || d < depth) {
				deepened[oid] = true
				c := parseCommit(o.Data)
				for _, p := range c.Parents {
					walkCommit(p, d+1)
				}
			}
			return
		}
		send[oid] = true
		c := parseCommit(o.Data)
		repo.treeClosure(c.Tree, send)
		if depth > 0 && d >= depth {
			newShallow[oid] = true
			return
		}
		for _, p := range c.Parents {
			walkCommit(p, d+1)
		}
	}
	for _, wt := range wants {
		walkCommit(wt, 1)
	}
	// filter out anything already held
	var objs []*Obj
	var order []string
	for oid := range send {
		if !held[oid] {
			order = append(order, oid)
		}
	}
	sort.Strings(order)
	// commits first, then trees, then blobs/tags for tidy packs
	sort.SliceStable(order, func(i, j int) bool {
		return objRank(repo, order[i]) < objRank(repo, order[j])
	})
	for _, oid := range order {
		if o := repo.getObj(oid); o != nil {
			objs = append(objs, o)
		}
	}
	var ns, us []string
	for oid := range newShallow {
		ns = append(ns, oid)
	}
	for oid := range deepened {
		us = append(us, oid)
	}
	// remaining shallow boundaries stay shallow
	for _, s := range shallows {
		if !deepened[s] {
			ns = append(ns, s)
		}
	}
	sort.Strings(ns)
	sort.Strings(us)
	return objs, ns, us
}

func objRank(repo *Repo, oid string) int {
	o := repo.getObj(oid)
	if o == nil {
		return 9
	}
	switch o.Type {
	case "commit":
		return 0
	case "tree":
		return 1
	case "tag":
		return 2
	default:
		return 3
	}
}

func (a *App) gitReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	raw, _ := io.ReadAll(r.Body)
	packets, packBytes, err := decodePktLines(raw)
	if err != nil {
		writeErr(w, 400, "bad pkt-line")
		return
	}
	type cmd struct {
		oldOid, newOid, ref string
	}
	var cmds []cmd
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		s := string(p.payload)
		if i := strings.IndexByte(s, 0); i >= 0 {
			s = s[:i] // strip capabilities
		}
		s = strings.TrimSuffix(s, "\n")
		fields := strings.Fields(s)
		if len(fields) == 3 {
			cmds = append(cmds, cmd{fields[0], fields[1], fields[2]})
		}
	}
	// store incoming pack objects
	unpackOk := true
	if len(packBytes) > 0 {
		objs, perr := readPackObjects(packBytes, func(oid string) *Obj { return repo.getObj(oid) })
		if perr != nil {
			unpackOk = false
		} else {
			for oid, o := range objs {
				repo.Objects[oid] = o
			}
		}
	}
	wasEmpty := len(repo.Refs) == 0
	type result struct {
		ref, status string
	}
	var results []result
	for _, c := range cmds {
		cur := repo.Refs[c.ref]
		ok := false
		msg := ""
		if c.newOid == zeroOID {
			// delete
			if cur == "" {
				msg = "ref not found"
			} else if c.oldOid != zeroOID && c.oldOid != cur {
				msg = "stale old value"
			} else {
				delete(repo.Refs, c.ref)
				ok = true
			}
		} else {
			if repo.getObj(c.newOid) == nil {
				msg = "missing object"
			} else if c.oldOid == zeroOID {
				if cur != "" {
					msg = "already exists"
				} else {
					ok = true
				}
			} else {
				if cur != c.oldOid {
					msg = "stale old value"
				} else if !a.isDescendant(repo, c.newOid, c.oldOid) {
					msg = "non-fast-forward"
				} else {
					ok = true
				}
			}
		}
		if ok {
			if c.newOid != zeroOID {
				repo.Refs[c.ref] = c.newOid
			}
			results = append(results, result{c.ref, "ok " + c.ref})
			if wasEmpty && strings.HasPrefix(c.ref, "refs/heads/") {
				repo.DefaultBranch = strings.TrimPrefix(c.ref, "refs/heads/")
			}
		} else {
			results = append(results, result{c.ref, "ng " + c.ref + " " + msg})
		}
	}
	// build report-status stream (pkt-lined)
	var report []byte
	if unpackOk {
		report = append(report, pktText("unpack ok\n")...)
	} else {
		report = append(report, pktText("unpack bad pack\n")...)
	}
	for _, res := range results {
		report = append(report, pktText(res.status+"\n")...)
	}
	report = append(report, pktFlush...)
	// wrap in sideband band 1
	var out []byte
	out = append(out, sidebandChunk(report, 1)...)
	out = append(out, pktFlush...)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	w.Write(out)
}

// isDescendant reports whether oid is equal to or a descendant of ancestor.
func (a *App) isDescendant(repo *Repo, oid, ancestor string) bool {
	if oid == ancestor {
		return true
	}
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(cur string) bool {
		if cur == "" || seen[cur] {
			return false
		}
		seen[cur] = true
		if cur == ancestor {
			return true
		}
		o := repo.getObj(cur)
		if o == nil || o.Type != "commit" {
			return false
		}
		c := parseCommit(o.Data)
		for _, p := range c.Parents {
			if walk(p) {
				return true
			}
		}
		return false
	}
	return walk(oid)
}