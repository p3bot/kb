// Command tokenprobe runs every operation kb's GitHub drivers use, once each,
// with one token, and records the outcome, the permission GitHub says the
// endpoint needs, and the error shape when it is refused.
//
// Run it once with a fine-grained token holding Contents read and write and
// once with a Contents read-only token, against the same private repo.
//
// Usage: go run . -repo owner/name -token-file path [-work dir] [-label name] [run|cleanup]
// Refs written: refs/locks/token-probe, refs/locks/token-probe-git,
// refs/heads/token-probe-content. cleanup deletes them with the given token.
package main

import (
	"bytes"
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

const zeroOID = "0000000000000000000000000000000000000000"

var (
	owner, repo string
	token       string
	label       string
	gitDir      string
	client      = &http.Client{Timeout: 60 * time.Second}
	logf        *os.File
	repoID      string
	mainHead    string
	mainTree    string
)

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

type outcome struct {
	op, status, perms, detail string
	ok                        bool
}

var results []outcome

func report(o outcome) {
	results = append(results, o)
	mark := "ok  "
	if !o.ok {
		mark = "FAIL"
	}
	fmt.Printf("%s %-34s %-6s %-28s %s\n", mark, o.op, o.status, o.perms, trunc(o.detail, 160))
	b, _ := json.Marshal(map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano), "label": label, "op": o.op, "ok": o.ok,
		"status": o.status, "accepted_permissions": o.perms, "detail": o.detail})
	fmt.Fprintln(logf, string(b))
}

type resp struct {
	code  int
	body  []byte
	perms string
	rl    string
}

func api(method, path string, body any) resp {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://api.github.com"+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	hr, err := client.Do(req)
	if err != nil {
		return resp{code: -1, body: []byte(err.Error())}
	}
	defer hr.Body.Close()
	out, _ := io.ReadAll(hr.Body)
	if hr.StatusCode == 429 || (hr.StatusCode == 403 && bytes.Contains(bytes.ToLower(out), []byte("rate limit"))) {
		fmt.Fprintf(os.Stderr, "FATAL: rate limited (%d): %s\n", hr.StatusCode, trunc(string(out), 200))
		os.Exit(3)
	}
	if method != "GET" {
		time.Sleep(400 * time.Millisecond)
	}
	return resp{hr.StatusCode, out, hr.Header.Get("X-Accepted-GitHub-Permissions"),
		hr.Header.Get("X-RateLimit-Limit") + "/" + hr.Header.Get("X-RateLimit-Resource")}
}

func repoPath(p string) string { return fmt.Sprintf("/repos/%s/%s%s", owner, repo, p) }

func shaOf(b []byte) string {
	var r struct {
		Sha    string
		Object struct{ Sha string }
	}
	_ = json.Unmarshal(b, &r)
	if r.Sha != "" {
		return r.Sha
	}
	return r.Object.Sha
}

func rest(op, method, path string, body any, want int) (bool, []byte) {
	r := api(method, path, body)
	ok := r.code == want
	report(outcome{op: op, status: fmt.Sprint(r.code), perms: r.perms, detail: string(r.body), ok: ok})
	return ok, r.body
}

type RU struct {
	Name      string `json:"name"`
	BeforeOid string `json:"beforeOid"`
	AfterOid  string `json:"afterOid"`
	Force     bool   `json:"force"`
}

func updateRefs(op string, us ...RU) bool {
	q := `mutation($rid:ID!,$u:[RefUpdate!]!){updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}}`
	r := api("POST", "/graphql", map[string]any{"query": q, "variables": map[string]any{"rid": repoID, "u": us}})
	var g struct {
		Data   map[string]any   `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	_ = json.Unmarshal(r.body, &g)
	ok := r.code == 200 && len(g.Errors) == 0 && g.Data["updateRefs"] != nil
	report(outcome{op: op, status: fmt.Sprint(r.code), perms: r.perms, detail: string(r.body), ok: ok})
	return ok
}

func restRef(ref string) string {
	r := api("GET", repoPath("/git/ref/"+strings.TrimPrefix(ref, "refs/")), nil)
	if r.code != 200 {
		return ""
	}
	return shaOf(r.body)
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitDir
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitOp(op string, args ...string) (bool, string) {
	start := time.Now()
	out, err := git(args...)
	ok := err == nil
	report(outcome{op: op, status: fmt.Sprintf("%.1fs", time.Since(start).Seconds()), detail: out, ok: ok})
	return ok, out
}

func setup(work string) {
	gitDir = filepath.Join(work, "repo-"+owner+"-"+repo)
	must(os.MkdirAll(gitDir, 0o755))
	if _, err := os.Stat(filepath.Join(gitDir, ".git")); err != nil {
		_, err := git("init", "-q")
		must(err)
		_, _ = git("remote", "add", "origin", fmt.Sprintf("https://github.com/%s/%s.git", owner, repo))
	}
	f, err := os.OpenFile(filepath.Join(work, "probe.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	must(err)
	logf = f
}

func run() {
	fmt.Printf("token %s against %s/%s\n\n", label, owner, repo)
	fmt.Printf("%-4s %-34s %-6s %-28s %s\n", "", "operation", "status", "accepted permissions", "detail")

	// Reads.
	ok, b := rest("REST GET repo", "GET", repoPath(""), nil, 200)
	if ok {
		var r struct {
			Private     bool
			Permissions map[string]bool
		}
		_ = json.Unmarshal(b, &r)
		fmt.Printf("     private=%v permissions=%v\n", r.Private, r.Permissions)
	}
	r := api("GET", "/rate_limit", nil)
	fmt.Printf("     primary limit/resource: %s\n", r.rl)
	ok, b = rest("REST GET ref heads/main", "GET", repoPath("/git/ref/heads/main"), nil, 200)
	if ok {
		mainHead = shaOf(b)
	}
	ok, b = rest("REST GET commit main", "GET", repoPath("/git/commits/"+mainHead), nil, 200)
	if ok {
		var c struct{ Tree struct{ Sha string } }
		_ = json.Unmarshal(b, &c)
		mainTree = c.Tree.Sha
	}
	rest("REST GET contents config (head read)", "GET", repoPath("/contents/README.md"), nil, 200)
	gr := api("POST", "/graphql", map[string]any{"query": fmt.Sprintf(`query{repository(owner:%q,name:%q){id}}`, owner, repo)})
	var g struct {
		Data struct{ Repository struct{ ID string } }
	}
	_ = json.Unmarshal(gr.body, &g)
	repoID = g.Data.Repository.ID
	report(outcome{op: "GraphQL repository id", status: fmt.Sprint(gr.code), perms: gr.perms, detail: string(gr.body), ok: repoID != ""})
	gitOp("git ls-remote", "ls-remote", "origin")
	gitOp("git fetch main", "fetch", "-q", "origin", "main")
	if mainHead == "" || mainTree == "" {
		must(fmt.Errorf("cannot read main; nothing further to test"))
	}

	// Lock acquire, API driver: lock commit, then create the ref.
	now := time.Now().UTC().Format(time.RFC3339)
	lock1 := mainHead // fallback object for ref writes if commit creation is refused
	if ok, b := rest("REST POST commit (lock commit)", "POST", repoPath("/git/commits"),
		map[string]any{"message": `{"holder":"token-probe","label":"` + label + `","start":"` + now + `"}`, "tree": mainTree, "parents": []string{}}, 201); ok {
		lock1 = shaOf(b)
	}
	acquired, _ := rest("REST POST ref refs/locks (acquire)", "POST", repoPath("/git/refs"), map[string]string{"ref": "refs/locks/token-probe", "sha": lock1}, 201)
	if got := restRef("refs/locks/token-probe"); acquired && got != lock1 {
		report(outcome{op: "REST GET ref refs/locks (read-back)", detail: "got " + got + " want " + lock1})
	} else if acquired {
		report(outcome{op: "REST GET ref refs/locks (read-back)", status: "200", detail: lock1, ok: true})
	}

	// Renew: new commit, CAS through GraphQL.
	lock2 := lock1
	if ok, b := rest("REST POST commit (renew commit)", "POST", repoPath("/git/commits"),
		map[string]any{"message": `{"holder":"token-probe","renew":true}`, "tree": mainTree, "parents": []string{}}, 201); ok {
		lock2 = shaOf(b)
	}
	cur := restRef("refs/locks/token-probe")
	if cur == "" {
		cur = lock1
	}
	if updateRefs("GraphQL updateRefs renew (CAS)", RU{"refs/locks/token-probe", cur, lock2, true}) {
		cur = lock2
	}
	updateRefs("GraphQL updateRefs wrong beforeOid (expect refusal)", RU{"refs/locks/token-probe", mainHead, lock1, true})

	// Checkin shape on a scratch branch: blob, tree, commit, then one atomic
	// updateRefs moving the branch and deleting the lock.
	updateRefs("GraphQL updateRefs create branch", RU{"refs/heads/token-probe-content", zeroOID, mainHead, true})
	content := mainHead
	blob := ""
	if ok, b := rest("REST POST blob", "POST", repoPath("/git/blobs"),
		map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("probe " + now + "\n")), "encoding": "base64"}, 201); ok {
		blob = shaOf(b)
	}
	tree := mainTree
	if blob != "" {
		if ok, b := rest("REST POST tree (base_tree)", "POST", repoPath("/git/trees"), map[string]any{"base_tree": mainTree,
			"tree": []map[string]string{{"path": ".docstore/.meta/a7/tokenprobe.cue", "mode": "100644", "type": "blob", "sha": blob}}}, 201); ok {
			tree = shaOf(b)
		}
	} else {
		rest("REST POST tree (base_tree)", "POST", repoPath("/git/trees"), map[string]any{"base_tree": mainTree,
			"tree": []map[string]string{{"path": "x.md", "mode": "100644", "type": "blob", "sha": mainTree}}}, 201)
	}
	if ok, b := rest("REST POST commit (content)", "POST", repoPath("/git/commits"), map[string]any{"message": "kb: token probe\n", "tree": tree, "parents": []string{mainHead},
		"author": map[string]string{"name": "kb", "email": "kb@p3bot.invalid", "date": now}}, 201); ok {
		content = shaOf(b)
	}
	branch := restRef("refs/heads/token-probe-content")
	if branch == "" {
		branch = mainHead
	}
	updateRefs("GraphQL updateRefs checkin (atomic)", RU{"refs/heads/token-probe-content", branch, content, true}, RU{"refs/locks/token-probe", cur, zeroOID, true})
	fmt.Printf("     after checkin: branch=%.8s lock=%q\n", restRef("refs/heads/token-probe-content"), restRef("refs/locks/token-probe"))

	// Git transport driver over HTTPS: create, atomic CAS, conditional delete.
	gc, err := git("-c", "user.name=kb", "-c", "user.email=kb@p3bot.invalid", "commit-tree", mainTree, "-m", `{"holder":"token-probe-git"}`)
	must(err)
	gc2, err := git("-c", "user.name=kb", "-c", "user.email=kb@p3bot.invalid", "commit-tree", mainTree, "-m", `{"holder":"token-probe-git","renew":true}`)
	must(err)
	gitOp("git push create refs/locks", "push", "--porcelain", "origin", gc+":refs/locks/token-probe-git")
	gitOp("git push --force-with-lease renew", "push", "--porcelain", "--force-with-lease=refs/locks/token-probe-git:"+gc, "origin", "+"+gc2+":refs/locks/token-probe-git")
	gitOp("git push --atomic delete with lease", "push", "--porcelain", "--atomic", "--force-with-lease=refs/locks/token-probe-git:"+gc2, "origin", ":refs/locks/token-probe-git")
	gitOp("git ls-remote refs/locks (after)", "ls-remote", "origin", "refs/locks/*", "refs/heads/token-probe-content")

	fails := 0
	for _, o := range results {
		if !o.ok {
			fails++
		}
	}
	fmt.Printf("\n%d operations, %d refused or failed\n", len(results), fails)
}

func cleanup() {
	for _, ref := range []string{"refs/locks/token-probe", "refs/locks/token-probe-git", "refs/heads/token-probe-content"} {
		out, err := git("push", "origin", ":"+ref)
		fmt.Printf("delete %s: err=%v %s\n", ref, err, trunc(out, 120))
	}
	out, _ := git("ls-remote", "origin")
	fmt.Println("remaining refs:\n" + out)
}

func main() {
	repoFlag := flag.String("repo", "", "owner/name (required)")
	tokenFile := flag.String("token-file", "", "file holding the token (required)")
	work := flag.String("work", "work", "working directory")
	flag.StringVar(&label, "label", "", "label recorded in the log (default: token file name)")
	flag.Parse()
	var ok bool
	owner, repo, ok = strings.Cut(*repoFlag, "/")
	if !ok || *tokenFile == "" {
		must(fmt.Errorf("-repo owner/name and -token-file are required"))
	}
	var b []byte
	var err error
	if *tokenFile == "gh" { // the gh CLI's token, for a classic-token baseline
		b, err = exec.Command("gh", "auth", "token").Output()
	} else {
		b, err = os.ReadFile(*tokenFile)
	}
	must(err)
	token = strings.TrimSpace(string(b))
	if label == "" {
		label = filepath.Base(*tokenFile)
	}
	w, err := filepath.Abs(*work)
	must(err)
	must(os.MkdirAll(w, 0o755))
	setup(w)
	switch flag.Arg(0) {
	case "", "run":
		run()
	case "cleanup":
		cleanup()
	default:
		must(fmt.Errorf("unknown command %q", flag.Arg(0)))
	}
}
