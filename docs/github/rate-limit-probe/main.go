// Command rateprobe measures which GitHub write classes count toward the
// secondary content-generating limit (80 per minute, 500 per hour).
//
// GET /rate_limit reports only primary limits, so the only observable is a
// refusal: each probe paces one write class under the per-minute limit and
// runs past the hourly one. A 403 or 429 near 500 means the class counts.
//
// Usage: go run . [-work DIR] [-n 600] p1|p2|p3|p4|p5|p6|cleanup ...
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

const (
	owner   = "grantcarthew"
	repo    = "pentest"
	zeroOID = "0000000000000000000000000000000000000000"
	// One write per second keeps every probe at 60 per minute, under the
	// documented 80, so only the hourly limit can trip.
	interval = time.Second
)

var (
	token   string
	workDir string
	gitDir  string
	logF    *os.File
	probe   string
	client  = &http.Client{Timeout: 60 * time.Second}
	nWrites = flag.Int("n", 600, "writes per limit probe")
	repoID  string
	head    string
	tree    string
)

type entry struct {
	T       time.Time `json:"t"`
	Probe   string    `json:"probe"`
	I       int       `json:"i"`
	Op      string    `json:"op"`
	Status  int       `json:"status"`
	Ms      int64     `json:"ms"`
	Retry   string    `json:"retryAfter,omitempty"`
	Remain  string    `json:"primaryRemaining,omitempty"`
	Limited bool      `json:"limited,omitempty"`
	Body    string    `json:"body,omitempty"`
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(2)
	}
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func record(e entry) {
	e.T = time.Now()
	e.Probe = probe
	b, _ := json.Marshal(e)
	_, _ = logF.Write(append(b, '\n'))
}

func isLimited(status int, body []byte) bool {
	lb := bytes.ToLower(body)
	if status == 429 || (status == 403 && bytes.Contains(lb, []byte("rate limit"))) {
		return true
	}
	return bytes.Contains(body, []byte("RATE_LIMITED")) || bytes.Contains(lb, []byte("secondary rate limit"))
}

// trip records the refusal and exits 3, so a caller can wait out the cool-down.
func trip(i int, op string, status int, retry string, body []byte) {
	record(entry{I: i, Op: op, Status: status, Retry: retry, Limited: true, Body: trunc(string(body), 500)})
	fmt.Printf("TRIPPED probe=%s write=%d op=%s status=%d retry-after=%q\n  %s\n", probe, i, op, status, retry, trunc(string(body), 300))
	os.Exit(3)
}

func api(i int, op, method, path string, body any) (int, []byte) {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "https://api.github.com"+path, r)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		record(entry{I: i, Op: op, Status: -1, Body: err.Error()})
		return -1, []byte(err.Error())
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if isLimited(resp.StatusCode, out) {
		trip(i, op, resp.StatusCode, resp.Header.Get("Retry-After"), out)
	}
	e := entry{I: i, Op: op, Status: resp.StatusCode, Ms: time.Since(t0).Milliseconds(), Remain: resp.Header.Get("X-RateLimit-Remaining")}
	if resp.StatusCode >= 300 {
		e.Body = trunc(string(out), 300)
	}
	record(e)
	return resp.StatusCode, out
}

func gql(i int, op, query string, vars map[string]any) (int, []byte) {
	code, b := api(i, op, "POST", "/graphql", map[string]any{"query": query, "variables": vars})
	if code == 200 && bytes.Contains(b, []byte(`"errors"`)) {
		return 422, b
	}
	return code, b
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

func setup() {
	out, err := exec.Command("gh", "auth", "token").Output()
	must(err)
	token = strings.TrimSpace(string(out))
	code, b := api(0, "setup:head", "GET", fmt.Sprintf("/repos/%s/%s/commits/main", owner, repo), nil)
	if code != 200 {
		must(fmt.Errorf("read main: %d %s", code, trunc(string(b), 200)))
	}
	var c struct {
		Sha    string
		Commit struct{ Tree struct{ Sha string } }
	}
	must(json.Unmarshal(b, &c))
	head, tree = c.Sha, c.Commit.Tree.Sha
	_, b = gql(0, "setup:repoid", `query{repository(owner:"`+owner+`",name:"`+repo+`"){id}}`, nil)
	var r struct {
		Data struct{ Repository struct{ ID string } }
	}
	must(json.Unmarshal(b, &r))
	repoID = r.Data.Repository.ID
}

// paced runs n writes at one per interval, failing loudly on any write the
// host rejects for a reason other than a rate limit.
func paced(n int, write func(i int) (string, int, []byte, bool)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	start := time.Now()
	for i := 1; i <= n; i++ {
		<-t.C
		op, code, body, ok := write(i)
		if !ok {
			must(fmt.Errorf("write %d (%s) failed: %d %s", i, op, code, trunc(string(body), 300)))
		}
		if i%50 == 0 {
			fmt.Printf("  %s: %d/%d writes, %s elapsed\n", probe, i, n, time.Since(start).Round(time.Second))
		}
	}
	fmt.Printf("PASSED probe=%s writes=%d elapsed=%s with no refusal\n", probe, n, time.Since(start).Round(time.Second))
}

func refPath(ref string) string {
	return fmt.Sprintf("/repos/%s/%s/git/refs/%s", owner, repo, strings.TrimPrefix(ref, "refs/"))
}

// p1 counts the API writes in one exclusive one-shot metadata change: take
// the lock, write the row, and release the lock in one atomic update.
func p1() {
	const branch = "refs/heads/probe-content"
	const lock = "refs/locks/sv9qk2m7xp"
	code, b := api(0, "setup:branch", "POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]string{"ref": branch, "sha": head})
	if code != 201 {
		must(fmt.Errorf("create branch: %d %s", code, trunc(string(b), 200)))
	}
	writes := 0
	step := func(op string, code int, b []byte, want int) []byte {
		writes++
		if code != want {
			must(fmt.Errorf("%s: %d %s", op, code, trunc(string(b), 300)))
		}
		fmt.Printf("  write %d: %s -> %d\n", writes, op, code)
		return b
	}
	var obj struct{ Sha string }
	path := fmt.Sprintf("/repos/%s/%s/git/", owner, repo)

	code, b = api(1, "lock:commit", "POST", path+"commits", map[string]any{"tree": tree, "message": `{"holder":"probe","host":"gdc"}`})
	must(json.Unmarshal(step("acquire: create lock commit", code, b, 201), &obj))
	lockSha := obj.Sha
	code, b = gql(2, "lock:create", `mutation($rid:ID!,$u:[RefUpdate!]!){updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}}`,
		map[string]any{"rid": repoID, "u": []map[string]any{{"name": lock, "beforeOid": zeroOID, "afterOid": lockSha}}})
	step("acquire: create lock ref (updateRefs, absent -> commit)", code, b, 200)

	code, b = api(3, "row:blob", "POST", path+"blobs", map[string]string{"content": "status: \"accepted\"\n", "encoding": "utf-8"})
	must(json.Unmarshal(step("checkin: row blob", code, b, 201), &obj))
	blob := obj.Sha
	code, b = api(4, "row:tree", "POST", path+"trees", map[string]any{"base_tree": tree, "tree": []map[string]string{
		{"path": ".docstore/.meta/a7/sv9qk2m7xp.cue", "mode": "100644", "type": "blob", "sha": blob},
	}})
	must(json.Unmarshal(step("checkin: tree (nested path, one call)", code, b, 201), &obj))
	newTree := obj.Sha
	code, b = api(5, "row:commit", "POST", path+"commits", map[string]any{"tree": newTree, "parents": []string{head}, "message": "kb: meta sv9qk2m7xp"})
	must(json.Unmarshal(step("checkin: content commit", code, b, 201), &obj))
	newHead := obj.Sha
	code, b = gql(6, "checkin:updaterefs", `mutation($rid:ID!,$u:[RefUpdate!]!){updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}}`,
		map[string]any{"rid": repoID, "u": []map[string]any{
			{"name": branch, "beforeOid": head, "afterOid": newHead},
			{"name": lock, "beforeOid": lockSha, "afterOid": zeroOID},
		}})
	step("checkin: updateRefs (branch head -> new, lock -> absent)", code, b, 200)

	_, b = api(0, "verify:branch", "GET", refPath(branch), nil)
	got := strings.Contains(string(b), newHead)
	code, _ = api(0, "verify:lock", "GET", refPath(lock), nil)
	fmt.Printf("RESULT p1: %d API writes per one-shot metadata change; branch moved=%v lock absent=%v\n", writes, got, code == 404)
	api(0, "cleanup:branch", "DELETE", refPath(branch), nil)
}

// p2: REST ref create and delete, alternating.
func p2() {
	const ref = "refs/locks/probe-rest"
	paced(*nWrites, func(i int) (string, int, []byte, bool) {
		if i%2 == 1 {
			code, b := api(i, "rest:create", "POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]string{"ref": ref, "sha": head})
			return "rest:create", code, b, code == 201
		}
		code, b := api(i, "rest:delete", "DELETE", refPath(ref), nil)
		return "rest:delete", code, b, code == 204
	})
}

// p3: Git Data object creation, cycling blob, tree, and commit.
func p3() {
	path := fmt.Sprintf("/repos/%s/%s/git/", owner, repo)
	var blob, tr string
	paced(*nWrites, func(i int) (string, int, []byte, bool) {
		var obj struct{ Sha string }
		switch i % 3 {
		case 1:
			code, b := api(i, "gitdata:blob", "POST", path+"blobs", map[string]string{"content": fmt.Sprintf("probe %d %d\n", i, time.Now().UnixNano()), "encoding": "utf-8"})
			_ = json.Unmarshal(b, &obj)
			blob = obj.Sha
			return "gitdata:blob", code, b, code == 201
		case 2:
			code, b := api(i, "gitdata:tree", "POST", path+"trees", map[string]any{"base_tree": tree, "tree": []map[string]string{
				{"path": ".docstore/.meta/zz/probe.cue", "mode": "100644", "type": "blob", "sha": blob},
			}})
			_ = json.Unmarshal(b, &obj)
			tr = obj.Sha
			return "gitdata:tree", code, b, code == 201
		default:
			code, b := api(i, "gitdata:commit", "POST", path+"commits", map[string]any{"tree": tr, "parents": []string{head}, "message": fmt.Sprintf("probe %d", i)})
			return "gitdata:commit", code, b, code == 201
		}
	})
}

// p4: GraphQL updateRefs create and delete, alternating.
func p4() {
	const ref = "refs/locks/probe-gql"
	const q = `mutation($rid:ID!,$u:[RefUpdate!]!){updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}}`
	paced(*nWrites, func(i int) (string, int, []byte, bool) {
		before, after := zeroOID, head
		if i%2 == 0 {
			before, after = head, zeroOID
		}
		code, b := gql(i, "gql:updaterefs", q, map[string]any{"rid": repoID, "u": []map[string]any{{"name": ref, "beforeOid": before, "afterOid": after}}})
		return "gql:updaterefs", code, b, code == 200
	})
}

// p5: git transport pushes over HTTPS, then API writes to see whether the
// pushes spent API budget. Pushes are unpaced; each costs about 1.6 s.
func p5() {
	must(os.MkdirAll(gitDir, 0o755))
	if _, err := os.Stat(filepath.Join(gitDir, ".git")); err != nil {
		_, err := git("init", "-q")
		must(err)
		_, _ = git("remote", "add", "origin", fmt.Sprintf("https://github.com/%s/%s.git", owner, repo))
		o, err := git("fetch", "-q", "--filter=blob:none", "origin", "main")
		if err != nil {
			must(fmt.Errorf("fetch: %s", o))
		}
	}
	const ref = "refs/locks/probe-git"
	start := time.Now()
	for i := 1; i <= *nWrites; i++ {
		spec, op := head+":"+ref, "git:create"
		if i%2 == 0 {
			spec, op = ":"+ref, "git:delete"
		}
		t0 := time.Now()
		o, err := git("push", "-q", "--atomic", "origin", spec)
		status := 0
		if err != nil {
			status = 1
			if strings.Contains(strings.ToLower(o), "rate limit") || strings.Contains(o, "429") {
				trip(i, op, 429, "", []byte(o))
			}
			must(fmt.Errorf("push %d: %s", i, o))
		}
		record(entry{I: i, Op: op, Status: status, Ms: time.Since(t0).Milliseconds()})
		if i%50 == 0 {
			fmt.Printf("  p5: %d/%d pushes, %s elapsed\n", i, *nWrites, time.Since(start).Round(time.Second))
		}
	}
	fmt.Printf("  p5: %d pushes in %s; now %d API writes to test the budget\n", *nWrites, time.Since(start).Round(time.Second), 10)
	const aref = "refs/locks/probe-after-git"
	for i := 1; i <= 10; i++ {
		var code int
		var b []byte
		if i%2 == 1 {
			code, b = api(i, "after:create", "POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]string{"ref": aref, "sha": head})
		} else {
			code, b = api(i, "after:delete", "DELETE", refPath(aref), nil)
		}
		if code >= 300 {
			must(fmt.Errorf("after write %d: %d %s", i, code, trunc(string(b), 200)))
		}
		time.Sleep(interval)
	}
	fmt.Printf("PASSED probe=p5 pushes=%d elapsed=%s and 10 API writes afterwards with no refusal\n", *nWrites, time.Since(start).Round(time.Second))
}

// p6 is the control: issue comments are documented as content-generating, so
// this run must trip near 500 or the probe cannot detect the limit at all.
func p6() {
	code, b := api(0, "setup:issue", "POST", fmt.Sprintf("/repos/%s/%s/issues", owner, repo), map[string]string{"title": "rate limit probe control", "body": "Deleted after the run."})
	if code != 201 {
		must(fmt.Errorf("create issue: %d %s", code, trunc(string(b), 200)))
	}
	var iss struct {
		Number int
		NodeID string `json:"node_id"`
	}
	must(json.Unmarshal(b, &iss))
	must(os.WriteFile(filepath.Join(workDir, "control-issue"), []byte(iss.NodeID), 0o600))
	fmt.Printf("  p6: control issue #%d\n", iss.Number)
	paced(*nWrites, func(i int) (string, int, []byte, bool) {
		code, b := api(i, "control:comment", "POST", fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, iss.Number), map[string]string{"body": fmt.Sprintf("probe %d", i)})
		return "control:comment", code, b, code == 201
	})
}

func cleanup() {
	for _, ref := range []string{"refs/locks/probe-rest", "refs/locks/probe-gql", "refs/locks/probe-git", "refs/locks/probe-after-git", "refs/locks/sv9qk2m7xp", "refs/heads/probe-content"} {
		code, _ := api(0, "cleanup:ref", "DELETE", refPath(ref), nil)
		fmt.Printf("  cleanup %s: %d\n", ref, code)
	}
	if id, err := os.ReadFile(filepath.Join(workDir, "control-issue")); err == nil {
		code, b := gql(0, "cleanup:issue", `mutation($id:ID!){deleteIssue(input:{issueId:$id}){clientMutationId}}`, map[string]any{"id": string(id)})
		fmt.Printf("  cleanup control issue: %d %s\n", code, trunc(string(b), 120))
		if code == 200 {
			_ = os.Remove(filepath.Join(workDir, "control-issue"))
		}
	}
}

func main() {
	wd := flag.String("work", ".", "directory for the log, git dir, and control issue id")
	flag.Parse()
	workDir, _ = filepath.Abs(*wd)
	gitDir = filepath.Join(workDir, "repo")
	must(os.MkdirAll(workDir, 0o755))
	var err error
	logF, err = os.OpenFile(filepath.Join(workDir, "probe.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	must(err)
	defer logF.Close()
	probes := map[string]func(){"p1": p1, "p2": p2, "p3": p3, "p4": p4, "p5": p5, "p6": p6, "cleanup": cleanup}
	for _, a := range flag.Args() {
		if _, ok := probes[a]; !ok {
			must(fmt.Errorf("unknown probe %q", a))
		}
	}
	probe = "setup"
	setup()
	for _, a := range flag.Args() {
		probe = a
		fmt.Printf("== %s (%s)\n", a, time.Now().Format(time.RFC3339))
		probes[a]()
	}
}
