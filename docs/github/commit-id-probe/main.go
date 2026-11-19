// Command commitidprobe checks whether GitHub's REST Git Data API keeps the
// object IDs of blobs, trees, and commits built locally with git, when the
// commit's author and committer are given explicitly.
//
// kb's API driver builds every commit in its clone, uploads the same objects
// through REST, and then moves the branch with GraphQL updateRefs. Its clone
// holds its own writes only if GitHub returns the commit ID git computed.
//
// Usage: go run . [-repo owner/name] [-token-file path] [-work dir] [case...]
// The token comes from -token-file, else GITHUB_TOKEN, else `gh auth token`.
// Objects are created unreferenced; no ref is written.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var (
	owner, repo string
	token       string
	gitDir      string
	client      = &http.Client{Timeout: 60 * time.Second}
	mainHead    string
	mainTree    string
	logf        *os.File
	mismatches  int
)

type ident struct {
	name, email string
	when        time.Time
}

// gitDate is git's raw date form, which keeps the offset exactly.
func (i ident) gitDate() string { return fmt.Sprintf("@%d %s", i.when.Unix(), i.when.Format("-0700")) }

// isoDate is the ISO 8601 form the REST API takes.
func (i ident) isoDate() string { return i.when.Format(time.RFC3339) }

type change struct {
	path    string
	mode    string // 100644 or 100755
	content []byte
	del     bool
	utf8    bool // upload with encoding utf-8 instead of base64
}

type tc struct {
	name      string
	note      string
	pre       []change // applied first, giving the base tree for changes
	changes   []change
	parents   string // "main", "none", "merge"
	author    ident
	committer *ident // nil: omit the committer and let GitHub set it
	message   string
	info      bool // informational: a mismatch is expected and not counted
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(2)
	}
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func record(v map[string]any) {
	v["t"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(v)
	fmt.Fprintln(logf, string(b))
}

func api(method, path string, body any) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://api.github.com"+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := client.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 429 || (resp.StatusCode == 403 && bytes.Contains(bytes.ToLower(out), []byte("rate limit"))) {
		fmt.Fprintf(os.Stderr, "FATAL: rate limited (%d): %s\n", resp.StatusCode, trunc(string(out), 200))
		os.Exit(3)
	}
	if method != "GET" {
		time.Sleep(400 * time.Millisecond) // stay well under 900 REST points per minute
	}
	return resp.StatusCode, out
}

func repoPath(p string) string { return fmt.Sprintf("/repos/%s/%s%s", owner, repo, p) }

func shaOf(b []byte) string {
	var r struct{ Sha string }
	_ = json.Unmarshal(b, &r)
	return r.Sha
}

func gitEnv(extra ...string) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return append(append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
	), extra...)
}

func gitIn(stdin []byte, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitDir
	cmd.Env = gitEnv(env...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func gitMust(args ...string) string {
	out, err := gitIn(nil, nil, args...)
	must(err)
	return out
}

func setup(work string) {
	gitDir = filepath.Join(work, "repo-"+owner+"-"+repo)
	must(os.MkdirAll(gitDir, 0o755))
	if _, err := os.Stat(filepath.Join(gitDir, ".git")); err != nil {
		gitMust("init", "-q")
		gitMust("remote", "add", "origin", fmt.Sprintf("https://github.com/%s/%s.git", owner, repo))
	}
	gitMust("fetch", "-q", "origin", "main")
	mainHead = gitMust("rev-parse", "FETCH_HEAD")
	mainTree = gitMust("rev-parse", "FETCH_HEAD^{tree}")
	f, err := os.OpenFile(filepath.Join(work, "probe.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	must(err)
	logf = f
	fmt.Printf("repo %s/%s main %s tree %s\n", owner, repo, mainHead[:8], mainTree[:8])
}

// localBuild writes blobs, a tree, and a commit with git and returns their IDs.
func localBuild(c tc, parents []string) (blobs []string, tree, commit, raw string, err error) {
	idx := filepath.Join(gitDir, ".git", "probe-index")
	_ = os.Remove(idx)
	env := []string{"GIT_INDEX_FILE=" + idx}
	if c.parents == "none" {
		if _, err = gitIn(nil, env, "read-tree", "--empty"); err != nil {
			return
		}
	} else if _, err = gitIn(nil, env, "read-tree", mainTree); err != nil {
		return
	}
	for _, ch := range append(append([]change{}, c.pre...), c.changes...) {
		if ch.del {
			blobs = append(blobs, "")
			if _, err = gitIn(nil, env, "update-index", "--force-remove", ch.path); err != nil {
				return
			}
			continue
		}
		var sha string
		if sha, err = gitIn(ch.content, nil, "hash-object", "-w", "--no-filters", "--stdin"); err != nil {
			return
		}
		blobs = append(blobs, sha)
		if _, err = gitIn(nil, env, "update-index", "--add", "--cacheinfo", ch.mode+","+sha+","+ch.path); err != nil {
			return
		}
	}
	if tree, err = gitIn(nil, env, "write-tree"); err != nil {
		return
	}
	committer := c.author
	if c.committer != nil {
		committer = *c.committer
	}
	cenv := []string{
		"GIT_AUTHOR_NAME=" + c.author.name, "GIT_AUTHOR_EMAIL=" + c.author.email, "GIT_AUTHOR_DATE=" + c.author.gitDate(),
		"GIT_COMMITTER_NAME=" + committer.name, "GIT_COMMITTER_EMAIL=" + committer.email, "GIT_COMMITTER_DATE=" + committer.gitDate(),
	}
	args := []string{"commit-tree", tree, "-F", "-"}
	for _, p := range parents {
		args = append(args, "-p", p)
	}
	if commit, err = gitIn([]byte(c.message), cenv, args...); err != nil {
		return
	}
	raw, err = gitIn(nil, nil, "cat-file", "commit", commit)
	return
}

// remoteBuild uploads the same objects through REST and returns GitHub's IDs.
func remoteBuild(c tc, parents []string) (blobs []string, tree, commit string, detail map[string]any) {
	detail = map[string]any{}
	baseTree := mainTree
	if len(c.pre) > 0 {
		pb, pt, d := remoteTree(c.pre, mainTree)
		blobs = append(blobs, pb...)
		if pt == "" {
			for k, v := range d {
				detail["pre_"+k] = v
			}
			return
		}
		baseTree = pt
	}
	if c.parents == "none" {
		baseTree = ""
	}
	cb, tree, d := remoteTree(c.changes, baseTree)
	blobs = append(blobs, cb...)
	for k, v := range d {
		detail[k] = v
	}
	if tree == "" {
		return
	}
	commit, detail = remoteCommit(c, tree, parents, detail)
	return
}

// remoteTree uploads blobs and one tree on base (none when base is "").
func remoteTree(changes []change, base string) (blobs []string, tree string, detail map[string]any) {
	detail = map[string]any{}
	var entries []map[string]any
	for _, ch := range changes {
		if ch.del {
			blobs = append(blobs, "")
			entries = append(entries, map[string]any{"path": ch.path, "mode": ch.mode, "type": "blob", "sha": nil})
			continue
		}
		body := map[string]string{"content": base64.StdEncoding.EncodeToString(ch.content), "encoding": "base64"}
		if ch.utf8 {
			body = map[string]string{"content": string(ch.content), "encoding": "utf-8"}
		}
		code, b := api("POST", repoPath("/git/blobs"), body)
		if code != 201 {
			detail["blob_error"] = fmt.Sprintf("%d %s", code, trunc(string(b), 200))
			return
		}
		sha := shaOf(b)
		blobs = append(blobs, sha)
		entries = append(entries, map[string]any{"path": ch.path, "mode": ch.mode, "type": "blob", "sha": sha})
	}
	tb := map[string]any{"tree": entries}
	if base != "" {
		tb["base_tree"] = base
	}
	code, b := api("POST", repoPath("/git/trees"), tb)
	if code != 201 {
		detail["tree_error"] = fmt.Sprintf("%d %s", code, trunc(string(b), 200))
		return
	}
	tree = shaOf(b)
	return
}

func remoteCommit(c tc, tree string, parents []string, detail map[string]any) (commit string, _ map[string]any) {
	cb := map[string]any{
		"message": c.message,
		"tree":    tree,
		"parents": parents,
		"author":  map[string]string{"name": c.author.name, "email": c.author.email, "date": c.author.isoDate()},
	}
	if c.committer != nil {
		cb["committer"] = map[string]string{"name": c.committer.name, "email": c.committer.email, "date": c.committer.isoDate()}
	}
	code, b := api("POST", repoPath("/git/commits"), cb)
	if code != 201 {
		detail["commit_error"] = fmt.Sprintf("%d %s", code, trunc(string(b), 200))
		return "", detail
	}
	var r struct {
		Sha          string
		Message      string
		Author       map[string]any
		Committer    map[string]any
		Verification struct {
			Verified bool
			Reason   string
		}
	}
	_ = json.Unmarshal(b, &r)
	commit = r.Sha
	detail["gh_message"] = r.Message
	detail["gh_author"] = r.Author
	detail["gh_committer"] = r.Committer
	detail["gh_verified"] = r.Verification.Verified
	detail["gh_verification_reason"] = r.Verification.Reason
	return commit, detail
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func cases() []tc {
	bne := time.FixedZone("AEST", 10*3600)
	ist := time.FixedZone("IST", 5*3600+30*60)
	nyc := time.FixedZone("EST", -5*3600)
	now := time.Now().Truncate(time.Second)
	kb := ident{"kb", "kb@p3bot.invalid", now.In(bne)}
	doc := ".docstore/designs/sv9qk2m7xp-store.md"
	row := ".docstore/.meta/a7/sv9qk2m7xp.cue"
	body := []byte("# Store Design\n\nA body edit at " + now.Format(time.RFC3339Nano) + "\n")
	rowb := []byte("created: \"2026-11-08T09:47:30+10:00\"\nstatus:  \"draft\"\norder:   \"a0\"\n")
	row2 := ".docstore/.meta/3f/hm4tq8wz3k.cue"
	att := ".docstore/designs/sv9qk2m7xp-att-diagram.png"
	attb := randBytes(2048)
	// pre gives every delete case a known base: two documents, an attachment,
	// and two rows in different shards, on top of main's tree.
	pre := []change{
		{path: doc, mode: "100644", content: body},
		{path: ".docstore/designs/hm4tq8wz3k-cli.md", mode: "100644", content: []byte("# CLI\n")},
		{path: att, mode: "100644", content: attb},
		{path: row, mode: "100644", content: rowb},
		{path: row2, mode: "100644", content: rowb},
	}
	basic := func(name, note, msg string) tc {
		return tc{name: name, note: note, changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main", author: kb, committer: &kb, message: msg}
	}
	return []tc{
		basic("basic", "one nested file on base_tree, author = committer, +10:00, message ends in newline", "kb: checkin sv9qk2m7xp\n"),
		basic("no-newline", "message without a trailing newline", "kb: checkin sv9qk2m7xp"),
		basic("multiline", "subject, body, blank lines, trailing space, trailers", "kb: checkin sv9qk2m7xp\n\nBody line one.  \nBody line two.\n\n\nkb-store: p3bot\nkb-gen: 7xq2\n"),
		basic("crlf-message", "CR LF line endings in the message", "kb: checkin\r\n\r\nbody\r\n"),
		basic("leading-blank", "message starting with blank lines", "\n\nkb: checkin sv9qk2m7xp\n"),
		{name: "unicode", note: "non-ASCII author name, email, and message", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main",
			author: ident{"Grànt Çarthew 測試", "grànt@p3bot.invalid", now.In(bne)}, committer: &kb, message: "kb: čheckin — 設計 ✓\n"},
		{name: "offsets", note: "author +05:30, committer -05:00, distinct identities", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main",
			author: ident{"Author A", "a@p3bot.invalid", now.Add(-time.Hour).In(ist)}, committer: &ident{"Committer B", "b@p3bot.invalid", now.In(nyc)}, message: "kb: offsets\n"},
		{name: "utc", note: "dates in UTC (Z)", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main",
			author: ident{"kb", "kb@p3bot.invalid", now.UTC()}, committer: &ident{"kb", "kb@p3bot.invalid", now.UTC()}, message: "kb: utc\n"},
		{name: "old-date", note: "author date in 2001, committer now", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main",
			author: ident{"kb", "kb@p3bot.invalid", time.Date(2001, 2, 3, 4, 5, 6, 0, bne)}, committer: &kb, message: "kb: old date\n"},
		{name: "checkin-shape", note: "body, row in shard directory, binary attachment: one checkin", changes: []change{
			{path: doc, mode: "100644", content: body},
			{path: row, mode: "100644", content: rowb},
			{path: ".docstore/designs/sv9qk2m7xp-att-diagram.png", mode: "100644", content: randBytes(4096)},
		}, parents: "main", author: kb, committer: &kb, message: "kb: checkin sv9qk2m7xp\n"},
		{name: "exec-mode", note: "file mode 100755", changes: []change{{path: ".docstore/tools/k2m7xpsv9q-run.md", mode: "100755", content: []byte("#!/bin/sh\necho hi\n")}}, parents: "main", author: kb, committer: &kb, message: "kb: mode\n"},
		{name: "crlf-utf8", note: "CR LF content, no final newline, uploaded with encoding utf-8", changes: []change{{path: doc, mode: "100644", content: []byte("line one\r\nline two\r\nno final newline"), utf8: true}}, parents: "main", author: kb, committer: &kb, message: "kb: crlf utf-8\n"},
		{name: "crlf-base64", note: "same CR LF content uploaded with base64", changes: []change{{path: doc, mode: "100644", content: []byte("line one\r\nline two\r\nno final newline")}}, parents: "main", author: kb, committer: &kb, message: "kb: crlf base64\n"},
		{name: "empty-file", note: "zero-byte file", changes: []change{{path: ".docstore/notes/x3k9wz4mtq-empty.md", mode: "100644", content: []byte{}}}, parents: "main", author: kb, committer: &kb, message: "kb: empty\n"},
		{name: "delete", note: "delete a file (sha null); its directory keeps other files", pre: pre, changes: []change{{path: doc, mode: "100644", del: true}}, parents: "main", author: kb, committer: &kb, message: "kb: delete\n"},
		{name: "delete-prune", note: "delete the only row in a shard directory, which is pruned", pre: pre, changes: []change{{path: row, mode: "100644", del: true}}, parents: "main", author: kb, committer: &kb, message: "kb: delete row\n"},
		{name: "delete-prune2", note: "delete every row, pruning .meta and its shards", pre: pre, changes: []change{{path: row, mode: "100644", del: true}, {path: row2, mode: "100644", del: true}}, parents: "main", author: kb, committer: &kb, message: "kb: delete rows\n"},
		{name: "move", note: "move a document and its row-less attachment to another directory in one tree", pre: pre, changes: []change{
			{path: doc, mode: "100644", del: true}, {path: ".docstore/archive/sv9qk2m7xp-store.md", mode: "100644", content: body},
			{path: att, mode: "100644", del: true}, {path: ".docstore/archive/sv9qk2m7xp-att-diagram.png", mode: "100644", content: attb},
		}, parents: "main", author: kb, committer: &kb, message: "kb: move\n"},
		{name: "root", note: "no parents, nested paths, no base_tree", changes: []change{{path: doc, mode: "100644", content: body}, {path: row, mode: "100644", content: rowb}}, parents: "none", author: kb, committer: &kb, message: "kb: root\n"},
		{name: "merge", note: "two parents: main and the root case's commit", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "merge", author: kb, committer: &kb, message: "kb: merge\n"},
		{name: "no-committer", note: "committer omitted: GitHub defaults it to the author; records signing", changes: []change{{path: doc, mode: "100644", content: body}}, parents: "main", author: kb, committer: nil, message: "kb: no committer\n", info: true},
	}
}

func main() {
	repoFlag := flag.String("repo", "grantcarthew/pentest", "owner/name")
	tokenFile := flag.String("token-file", "", "file holding the token")
	work := flag.String("work", "work", "working directory")
	flag.Parse()
	var ok bool
	owner, repo, ok = strings.Cut(*repoFlag, "/")
	if !ok {
		must(fmt.Errorf("-repo must be owner/name"))
	}
	switch {
	case *tokenFile != "":
		b, err := os.ReadFile(*tokenFile)
		must(err)
		token = strings.TrimSpace(string(b))
	case os.Getenv("GITHUB_TOKEN") != "":
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	default:
		b, err := exec.Command("gh", "auth", "token").Output()
		must(err)
		token = strings.TrimSpace(string(b))
	}
	w, err := filepath.Abs(*work)
	must(err)
	must(os.MkdirAll(w, 0o755))
	setup(w)

	want := map[string]bool{}
	for _, a := range flag.Args() {
		want[a] = true
	}
	all := cases()
	rootLocal, rootRemote := "", ""
	fmt.Printf("%-14s %-6s %-6s %-8s %s\n", "case", "blobs", "tree", "commit", "note")
	for _, c := range all {
		if len(want) > 0 && !want[c.name] {
			continue
		}
		lp, rp := []string{}, []string{}
		switch c.parents {
		case "main":
			lp, rp = []string{mainHead}, []string{mainHead}
		case "merge":
			if rootLocal == "" || rootLocal != rootRemote {
				fmt.Printf("%-14s skipped: needs the root case to match first\n", c.name)
				continue
			}
			lp, rp = []string{mainHead, rootLocal}, []string{mainHead, rootRemote}
		}
		lb, lt, lc, raw, err := localBuild(c, lp)
		must(err)
		rb, rt, rc, det := remoteBuild(c, rp)
		blobsOK := len(lb) == len(rb)
		for i := range lb {
			if i < len(rb) && lb[i] != rb[i] {
				blobsOK = false
			}
		}
		mark := func(b bool) string {
			if b {
				return "same"
			}
			return "DIFF"
		}
		commitOK := lc == rc
		if c.name == "root" {
			rootLocal, rootRemote = lc, rc
		}
		res := mark(commitOK)
		if !commitOK && c.info {
			res = "diff*"
		} else if !commitOK || !blobsOK || lt != rt {
			mismatches++
		}
		fmt.Printf("%-14s %-6s %-6s %-8s %s\n", c.name, mark(blobsOK), mark(lt == rt), res, c.note)
		for _, k := range []string{"blob_error", "tree_error", "commit_error"} {
			if v, ok := det[k]; ok {
				fmt.Printf("  %s: %v\n", k, v)
			}
		}
		if !commitOK && rc != "" {
			fmt.Printf("  local raw commit:\n    %s\n", strings.ReplaceAll(raw, "\n", "\n    "))
			fmt.Printf("  github: message=%q author=%v committer=%v verified=%v\n", det["gh_message"], det["gh_author"], det["gh_committer"], det["gh_verified"])
		}
		rec := map[string]any{"case": c.name, "note": c.note, "local_blobs": lb, "remote_blobs": rb, "local_tree": lt, "remote_tree": rt,
			"local_commit": lc, "remote_commit": rc, "local_raw": raw, "message": c.message}
		for k, v := range det {
			rec[k] = v
		}
		record(rec)
	}
	if mismatches > 0 {
		fmt.Printf("RESULT: %d case(s) differ\n", mismatches)
		os.Exit(1)
	}
	fmt.Println("RESULT: all IDs match")
}
