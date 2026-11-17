package main

// The in-memory model: users, repositories, issues, pull requests, comments,
// labels, and each repository's git object store. One store serves REST,
// GraphQL, MCP and the git smart-HTTP protocol, so every surface observes
// the same state and the same id sequences.

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The fixed cast of users. The API token belongs to the viewer; the other
// accounts exist so ownership checks can tell "another owner's repository"
// (a 404, never a 403) apart from the viewer's own.
type User struct {
	ID    int
	Login string
	Name  string
	Email string
}

var (
	viewerUser   = &User{ID: 1000, Login: "adaiversen", Name: "ada iversen", Email: "adaiversen@example.invalid"}
	teammateUser = &User{ID: 1001, Login: "quinnkeller", Name: "quinn keller", Email: "quinnkeller@example.invalid"}
	strangerUser = &User{ID: 1002, Login: "olgabauer", Name: "olga bauer", Email: "olgabauer@example.invalid"}
	orgLogin     = "faint-meadow-org"
	usersByLogin = map[string]*User{
		viewerUser.Login:   viewerUser,
		teammateUser.Login: teammateUser,
		strangerUser.Login: strangerUser,
	}
)

// GitObject is one object in a repository's object store.
type GitObject struct {
	Type string // commit, tree, blob, tag
	Data []byte
}

// oidOf hashes an object exactly as git does: sha1("<type> <size>\0" + data).
func oidOf(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

type Label struct {
	Name string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt time.Time
}

type Issue struct {
	ID        int
	Number    int
	Title     string
	Body      string
	State     string // open | closed
	Labels    []string
	Comments  []*Comment
	CreatedAt time.Time

	jsonCache []byte // serialised REST form, rebuilt on mutation
}

type Pull struct {
	ID             int
	Number         int
	Title          string
	Body           string
	State          string // open | closed
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergedAt       string
	MergeCommitSHA string
	CreatedAt      time.Time
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedAt     time.Time

	Labels      []*Label
	labelByName map[string]*Label

	Issues     map[int]*Issue
	Pulls      map[int]*Pull
	nextNumber int // shared issue/pull number sequence, starts at 1

	Objects map[string]*GitObject
	Refs    map[string]string // full ref name -> oid hex
	HeadRef string            // refs/heads/<default>

	// zstreams caches each object's exact pack stream bytes (oid -> zlib
	// stream), computed once with the byte-exact compressor.
	zstreams map[string][]byte

	jsonCache []byte
}

func (r *Repo) label(name string) *Label {
	if l, ok := r.labelByName[name]; ok {
		return l
	}
	return nil
}

func (r *Repo) addLabel(name string) *Label {
	if l := r.label(name); l != nil {
		return l
	}
	l := &Label{Name: name}
	r.Labels = append(r.Labels, l)
	if r.labelByName == nil {
		r.labelByName = map[string]*Label{}
	}
	r.labelByName[name] = l
	return l
}

// putObject stores an object and returns its oid.
func (r *Repo) putObject(typ string, data []byte) string {
	oid := oidOf(typ, data)
	if _, ok := r.Objects[oid]; !ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		r.Objects[oid] = &GitObject{Type: typ, Data: cp}
	}
	return oid
}

func (r *Repo) getObject(oid string) *GitObject {
	return r.Objects[oid]
}

// zstreamMu guards the per-repo stream caches (reads may race fills).
var zstreamMu sync.Mutex

// zstream returns the object's byte-exact zlib stream for pack building.
func (r *Repo) zstream(oid string) []byte {
	zstreamMu.Lock()
	if r.zstreams == nil {
		r.zstreams = map[string][]byte{}
	}
	z, ok := r.zstreams[oid]
	zstreamMu.Unlock()
	if ok {
		return z
	}
	obj := r.Objects[oid]
	if obj == nil {
		return nil
	}
	z = zdeflate6(obj.Data)
	zstreamMu.Lock()
	r.zstreams[oid] = z
	zstreamMu.Unlock()
	return z
}

// ---- git object construction and parsing ----

type TreeEntry struct {
	Mode string // "100644", "40000", ...
	Name string
	OID  string
}

// buildTree serialises tree entries. Entries must already be in git order
// (byte-wise by name, directories as name+"/"); callers sort with sortTreeEntries.
func buildTree(entries []TreeEntry) []byte {
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		b.Write(raw)
	}
	return []byte(b.String())
}

func sortTreeEntries(entries []TreeEntry) {
	sort.Slice(entries, func(i, j int) bool {
		ni, nj := entries[i].Name, entries[j].Name
		if entries[i].Mode == "40000" {
			ni += "/"
		}
		if entries[j].Mode == "40000" {
			nj += "/"
		}
		return ni < nj
	})
}

func parseTree(data []byte) []TreeEntry {
	var out []TreeEntry
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
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return out
}

type Commit struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

func buildCommit(c *Commit) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + c.Author + "\n")
	b.WriteString("committer " + c.Committer + "\n")
	b.WriteString("\n")
	b.WriteString(c.Message)
	return []byte(b.String())
}

func parseCommit(data []byte) *Commit {
	c := &Commit{}
	head, msg, _ := strings.Cut(string(data), "\n\n")
	c.Message = msg
	for _, line := range strings.Split(head, "\n") {
		k, v, _ := strings.Cut(line, " ")
		switch k {
		case "tree":
			c.Tree = v
		case "parent":
			c.Parents = append(c.Parents, v)
		case "author":
			c.Author = v
		case "committer":
			c.Committer = v
		}
	}
	return c
}

// identLine renders "Name <email> ts +0000" for commits this forge creates.
func identLine(u *User, ts time.Time) string {
	return fmt.Sprintf("%s <%s> %d +0000", u.Name, u.Email, ts.Unix())
}

// commitTimestamp extracts the epoch seconds from an ident line.
func commitTimestamp(ident string) int64 {
	// "... <email> 1735689600 +0000"
	parts := strings.Fields(ident)
	if len(parts) >= 2 {
		if v, err := strconv.ParseInt(parts[len(parts)-2], 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// ---- the store ----

type Store struct {
	mu          sync.RWMutex
	repos       map[string]*Repo // "owner/name"
	order       []*Repo          // creation order
	nextRepoID  int
	nextIssueID int // shared by issues, pull requests and comments
}

func newStore() *Store {
	return &Store{
		repos:       map[string]*Repo{},
		nextRepoID:  1,
		nextIssueID: 1,
	}
}

func repoKey(owner, name string) string { return owner + "/" + name }

func (s *Store) createRepo(owner, name string, private bool, description string) *Repo {
	r := &Repo{
		ID:            s.nextRepoID,
		Owner:         owner,
		Name:          name,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedAt:     time.Now(),
		Issues:        map[int]*Issue{},
		Pulls:         map[int]*Pull{},
		nextNumber:    1,
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		HeadRef:       "refs/heads/main",
	}
	s.nextRepoID++
	s.repos[repoKey(owner, name)] = r
	s.order = append(s.order, r)
	return r
}

func (s *Store) repo(owner, name string) *Repo {
	return s.repos[repoKey(owner, name)]
}

func (s *Store) deleteRepo(owner, name string) bool {
	key := repoKey(owner, name)
	r, ok := s.repos[key]
	if !ok {
		return false
	}
	delete(s.repos, key)
	for i, x := range s.order {
		if x == r {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return true
}

func (s *Store) createIssue(r *Repo, title, body string, labels []string) *Issue {
	is := &Issue{
		ID:        s.nextIssueID,
		Number:    r.nextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		Labels:    append([]string(nil), labels...),
		CreatedAt: time.Now(),
	}
	s.nextIssueID++
	r.nextNumber++
	r.Issues[is.Number] = is
	return is
}

func (s *Store) createPull(r *Repo, title, body, headRef, baseRef string) *Pull {
	// Issues and pull requests share one id sequence, as they do on GitHub
	// (a pull request is an issue with commits attached).
	p := &Pull{
		ID:        s.nextIssueID,
		Number:    r.nextNumber,
		Title:     title,
		Body:      body,
		State:     "open",
		HeadRef:   headRef,
		BaseRef:   baseRef,
		CreatedAt: time.Now(),
	}
	s.nextIssueID++
	r.nextNumber++
	r.Pulls[p.Number] = p
	return p
}

func (s *Store) createComment(s2 *Issue, body string) *Comment {
	// Comments draw from the same id sequence as issues and pull requests.
	c := &Comment{ID: s.nextIssueID, Body: body, CreatedAt: time.Now()}
	s.nextIssueID++
	s2.Comments = append(s2.Comments, c)
	return c
}

// findIssueByID locates an issue across repos by its global id (GraphQL node ids).
func (s *Store) findIssueByID(id int) (*Repo, *Issue) {
	for _, r := range s.order {
		for _, is := range r.Issues {
			if is.ID == id {
				return r, is
			}
		}
	}
	return nil, nil
}

func (s *Store) findPullByID(id int) (*Repo, *Pull) {
	for _, r := range s.order {
		for _, p := range r.Pulls {
			if p.ID == id {
				return r, p
			}
		}
	}
	return nil, nil
}

func (s *Store) findRepoByID(id int) *Repo {
	for _, r := range s.order {
		if r.ID == id {
			return r
		}
	}
	return nil
}
