package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Git object model: content-addressed blobs, trees, commits and tags.

type Obj struct {
	Type string
	Data []byte
}

func hashObj(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type treeEntry struct {
	Mode string
	Name string
	OID  string
}

func buildTree(entries []treeEntry) []byte {
	sort.Slice(entries, func(i, j int) bool {
		// git sorts tree entries by name with directories treated as name+"/"
		a, b := entries[i].Name, entries[j].Name
		if entries[i].Mode == "40000" {
			a += "/"
		}
		if entries[j].Mode == "40000" {
			b += "/"
		}
		return a < b
	})
	var buf []byte
	for _, e := range entries {
		oid, _ := hex.DecodeString(e.OID)
		buf = append(buf, []byte(e.Mode+" "+e.Name+"\x00")...)
		buf = append(buf, oid...)
	}
	return buf
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	i := 0
	for i < len(data) {
		sp := strings.IndexByte(string(data[i:]), ' ')
		if sp < 0 {
			break
		}
		mode := string(data[i : i+sp])
		i += sp + 1
		nul := strings.IndexByte(string(data[i:]), 0)
		if nul < 0 {
			break
		}
		name := string(data[i : i+nul])
		i += nul + 1
		if i+20 > len(data) {
			break
		}
		oid := hex.EncodeToString(data[i : i+20])
		i += 20
		out = append(out, treeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

type commitInfo struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func buildCommit(c commitInfo) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", c.Tree)
	for _, p := range c.Parents {
		fmt.Fprintf(&b, "parent %s\n", p)
	}
	fmt.Fprintf(&b, "author %s\n", c.Author)
	fmt.Fprintf(&b, "committer %s\n", c.Committer)
	b.WriteString("\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

func parseCommit(data []byte) commitInfo {
	var c commitInfo
	s := string(data)
	idx := strings.Index(s, "\n\n")
	head := s
	if idx >= 0 {
		head = s[:idx]
		c.Message = s[idx+2:]
	}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[9:]
		}
	}
	return c
}

// identity builds a git author/committer line with a deterministic timestamp.
func identity(name, email string, epoch int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", name, email, epoch)
}

func parseIdentEpoch(line string) int64 {
	// name <email> epoch tz
	parts := strings.Fields(line)
	if len(parts) >= 2 {
		if n, err := strconv.ParseInt(parts[len(parts)-2], 10, 64); err == nil {
			return n
		}
	}
	return 0
}

func identName(line string) string {
	if i := strings.Index(line, " <"); i >= 0 {
		return line[:i]
	}
	return line
}

func identEmail(line string) string {
	a := strings.Index(line, "<")
	b := strings.Index(line, ">")
	if a >= 0 && b > a {
		return line[a+1 : b]
	}
	return ""
}