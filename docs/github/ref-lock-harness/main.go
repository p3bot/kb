// Command lockrace stress-tests GitHub refs as a distributed lock primitive.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	owner   = "grantcarthew"
	repo    = "pentest"
	poolN   = 40
	perMin  = 60 // self-imposed API mutation budget per rolling minute
	zeroOID = "0000000000000000000000000000000000000000"
)

var (
	base   string
	gitDir string
	token  string
	client = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64}}
	st     state
	stPath string
	stMu   sync.Mutex
	failed bool
	gitSSH string
)

type state struct {
	RepoID    string      `json:"repoID"`
	Tree      string      `json:"tree"`
	Pool      []string    `json:"pool"`
	Mutations []time.Time `json:"mutations"`
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "FATAL:", err)
		os.Exit(2)
	}
}

func loadState() {
	b, err := os.ReadFile(stPath)
	if err == nil {
		must(json.Unmarshal(b, &st))
	}
}

func saveState() {
	stMu.Lock()
	defer stMu.Unlock()
	b, _ := json.MarshalIndent(st, "", " ")
	must(os.WriteFile(stPath, b, 0o600))
}

// budget blocks until n more mutations fit in the rolling one-minute window.
func budget(n int) {
	for {
		stMu.Lock()
		cut := time.Now().Add(-time.Minute)
		var keep []time.Time
		for _, t := range st.Mutations {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		st.Mutations = keep
		if len(keep)+n <= perMin {
			now := time.Now()
			for range n {
				st.Mutations = append(st.Mutations, now)
			}
			stMu.Unlock()
			saveState()
			return
		}
		wait := time.Until(keep[0].Add(time.Minute)) + 200*time.Millisecond
		stMu.Unlock()
		fmt.Printf("  (pacing: waiting %s for API budget)\n", wait.Round(time.Second))
		time.Sleep(wait)
	}
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
		fmt.Fprintf(os.Stderr, "FATAL: rate limited (%d) retry-after=%s: %s\n", resp.StatusCode, resp.Header.Get("Retry-After"), out)
		os.Exit(3)
	}
	return resp.StatusCode, out
}

func gql(query string, vars map[string]any) (int, []byte) {
	return api("POST", "/graphql", map[string]any{"query": query, "variables": vars})
}

type RU struct {
	Name      string `json:"name"`
	BeforeOid string `json:"beforeOid"`
	AfterOid  string `json:"afterOid"`
	Force     bool   `json:"force"`
}

const updateRefsQ = `mutation($rid:ID!,$u:[RefUpdate!]!){updateRefs(input:{repositoryId:$rid,refUpdates:$u}){clientMutationId}}`

func updateRefs(us ...RU) (bool, string) {
	code, b := gql(updateRefsQ, map[string]any{"rid": st.RepoID, "u": us})
	var r struct {
		Data   map[string]any   `json:"data"`
		Errors []map[string]any `json:"errors"`
	}
	_ = json.Unmarshal(b, &r)
	ok := code == 200 && len(r.Errors) == 0 && r.Data["updateRefs"] != nil
	return ok, fmt.Sprintf("%d %s", code, trunc(string(b), 300))
}

func restCreate(ref, sha string) (bool, string) {
	code, b := api("POST", fmt.Sprintf("/repos/%s/%s/git/refs", owner, repo), map[string]string{"ref": ref, "sha": sha})
	return code == 201, fmt.Sprintf("%d %s", code, trunc(string(b), 200))
}

func restGet(ref string) string {
	code, b := api("GET", fmt.Sprintf("/repos/%s/%s/git/ref/%s", owner, repo, strings.TrimPrefix(ref, "refs/")), nil)
	if code == 404 {
		return ""
	}
	var r struct {
		Object struct{ Sha string } `json:"object"`
	}
	_ = json.Unmarshal(b, &r)
	if r.Object.Sha == "" {
		return "ERR:" + trunc(string(b), 120)
	}
	return r.Object.Sha
}

// gqlGet reads a ref through GraphQL. Retained to reproduce finding F3:
// GraphQL cannot see refs outside refs/heads and refs/tags, so this returns
// "" for refs/locks/* even when the ref exists. Not used by the checks.
func gqlGet(ref string) string {
	q := `query($o:String!,$n:String!,$r:String!){repository(owner:$o,name:$n){ref(qualifiedName:$r){target{oid}}}}`
	_, b := gql(q, map[string]any{"o": owner, "n": repo, "r": ref})
	var r struct {
		Data struct {
			Repository struct {
				Ref *struct{ Target struct{ Oid string } } `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
	}
	_ = json.Unmarshal(b, &r)
	if r.Data.Repository.Ref == nil {
		return ""
	}
	return r.Data.Repository.Ref.Target.Oid
}

func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = gitDir
	cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+gitSSH, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func lsRemote(ref string) string {
	out, err := git("ls-remote", "origin", ref)
	if err != nil {
		return "ERR:" + out
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && f[1] == ref {
			return f[0]
		}
	}
	return ""
}

func forceSet(ref, sha string) {
	out, err := git("push", "--force", "origin", sha+":"+ref)
	if err != nil {
		must(fmt.Errorf("forceSet %s: %s", ref, out))
	}
}

func forceDel(ref string) {
	_, _ = git("push", "origin", ":"+ref)
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " | ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func short(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	if s == "" {
		return "(absent)"
	}
	return s
}

type result struct {
	ok         bool
	start, end time.Time
	detail     string
}

// race releases n goroutines simultaneously from a barrier.
func race(n int, f func(i int) (bool, string)) []result {
	res := make([]result, n)
	gate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			s := time.Now()
			ok, d := f(i)
			res[i] = result{ok, s, time.Now(), d}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	return res
}

// warm opens n idle HTTPS connections so racers start without TLS handshakes.
func warm(n int) {
	race(n, func(int) (bool, string) { api("GET", "/rate_limit", nil); return true, "" })
}

type tally struct {
	name                  string
	rounds, good, anomaly int
	maxSpread, minOverlap time.Duration
	allOverlap            bool
	notes                 map[string]int
}

func newTally(name string) *tally {
	return &tally{name: name, allOverlap: true, minOverlap: time.Hour, notes: map[string]int{}}
}

// check validates one race round: exactly one winner, and the remote ref equals expect(winner).
func (t *tally) check(res []result, expect func(w int) string, ref string) {
	t.rounds++
	var winners []int
	minS, maxS, minE := res[0].start, res[0].start, res[0].end
	for i, r := range res {
		if r.ok {
			winners = append(winners, i)
		} else {
			t.notes[classify(r.detail)]++
		}
		if r.start.Before(minS) {
			minS = r.start
		}
		if r.start.After(maxS) {
			maxS = r.start
		}
		if r.end.Before(minE) {
			minE = r.end
		}
	}
	spread := maxS.Sub(minS)
	overlap := minE.Sub(maxS) // >0 means every request was in flight simultaneously
	t.maxSpread = max(t.maxSpread, spread)
	t.minOverlap = min(t.minOverlap, overlap)
	if overlap <= 0 {
		t.allOverlap = false
	}
	ls, rg := lsRemote(ref), restGet(ref)
	status := "ok"
	if len(winners) != 1 {
		status = fmt.Sprintf("ANOMALY: %d winners %v", len(winners), winners)
		t.anomaly++
	} else if want := expect(winners[0]); ls != want || rg != want {
		status = fmt.Sprintf("ANOMALY: winner=%d want=%s ls-remote=%s rest=%s", winners[0], short(want), short(ls), short(rg))
		t.anomaly++
	} else {
		t.good++
	}
	if status != "ok" {
		failed = true
		for i, r := range res {
			fmt.Printf("    racer %d ok=%v %s\n", i, r.ok, r.detail)
		}
	}
	fmt.Printf("  round %2d: winners=%v spread=%s overlap=%s final=%s %s\n", t.rounds, winners, spread.Round(time.Millisecond), overlap.Round(time.Millisecond), short(ls), status)
}

func (t *tally) report() {
	fmt.Printf("== %s: rounds=%d correct=%d anomalies=%d max-start-spread=%s min-overlap=%s all-in-flight-together=%v\n",
		t.name, t.rounds, t.good, t.anomaly, t.maxSpread.Round(time.Millisecond), t.minOverlap.Round(time.Millisecond), t.allOverlap)
	keys := make([]string, 0, len(t.notes))
	for k := range t.notes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("   loser outcome %-40s x%d\n", k, t.notes[k])
	}
}

func classify(d string) string {
	switch {
	case strings.Contains(d, "stale info"):
		return "git: client-side lease reject (stale info)"
	case strings.Contains(d, "remote rejected"):
		return "git: SERVER-side reject (remote rejected)"
	case strings.HasPrefix(d, "422") && strings.Contains(d, "Reference already exists"):
		return "rest: 422 Reference already exists"
	case strings.Contains(d, "fetch first"), strings.Contains(d, "already exists"):
		return "git: client-side reject (exists/fetch first)"
	case strings.Contains(d, "failed to push"):
		return "git: other reject"
	case strings.HasPrefix(d, "422") && strings.Contains(d, "Reference already exists"):
		return "rest: 422 Reference already exists"
	case strings.HasPrefix(d, "422"):
		return "rest: 422 other"
	case strings.Contains(d, "Something went wrong"):
		return "gql: opaque 'Something went wrong'"
	case strings.Contains(d, `"errors"`):
		return "gql: other error"
	}
	return "other: " + trunc(d, 60)
}

func gitPushResult(args ...string) (bool, string) {
	out, err := git(args...)
	return err == nil, out
}

// ---------- tests ----------

const rounds = 8
const racers = 8

func pool(i int) string { return st.Pool[i] }

func testRestCreate() {
	t := newTally("T1 REST create race")
	ref := "refs/locks/race-rest"
	for range rounds {
		forceDel(ref)
		warm(racers)
		budget(racers)
		res := race(racers, func(i int) (bool, string) { return restCreate(ref, pool(i)) })
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testGqlCAS() {
	t := newTally("T2 GraphQL CAS race (renew/takeover)")
	ref := "refs/locks/race-gql"
	b := pool(30)
	for range rounds {
		forceSet(ref, b)
		warm(racers)
		budget(racers)
		res := race(racers, func(i int) (bool, string) { return updateRefs(RU{ref, b, pool(i), true}) })
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testGqlCreate() {
	t := newTally("T3 GraphQL create race (beforeOid=zero)")
	ref := "refs/locks/race-gqlc"
	for range rounds / 2 {
		forceDel(ref)
		warm(racers)
		budget(racers)
		res := race(racers, func(i int) (bool, string) { return updateRefs(RU{ref, zeroOID, pool(i), true}) })
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testReleaseVsTakeover() {
	t := newTally("T4 release (delete) vs takeover race")
	ref := "refs/locks/race-rvt"
	b := pool(30)
	expect := func(w int) string {
		if w%2 == 0 {
			return ""
		}
		return pool(w)
	}
	for range rounds / 2 {
		forceSet(ref, b)
		warm(racers)
		budget(racers)
		res := race(racers, func(i int) (bool, string) {
			if i%2 == 0 {
				return updateRefs(RU{ref, b, zeroOID, true})
			}
			return updateRefs(RU{ref, b, pool(i), true})
		})
		t.check(res, expect, ref)
	}
	forceDel(ref)
	t.report()
}

func testGitLease() {
	t := newTally("T5 git push --force-with-lease race")
	ref := "refs/locks/race-git"
	b := pool(30)
	for range rounds {
		forceSet(ref, b)
		res := race(racers, func(i int) (bool, string) {
			return gitPushResult("push", "--porcelain", "--force-with-lease="+ref+":"+b, "origin", pool(i)+":"+ref)
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testGitCreate() {
	t := newTally("T6 git push create race (no force)")
	ref := "refs/locks/race-gitc"
	for range rounds / 2 {
		forceDel(ref)
		res := race(racers, func(i int) (bool, string) {
			return gitPushResult("push", "--porcelain", "origin", pool(i)+":"+ref)
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testMixed() {
	t := newTally("T7 mixed REST/GraphQL/git create race")
	ref := "refs/locks/race-mix"
	n := 9
	for range rounds / 2 {
		forceDel(ref)
		warm(n)
		budget(6)
		res := race(n, func(i int) (bool, string) {
			switch i % 3 {
			case 0:
				return restCreate(ref, pool(i))
			case 1:
				return updateRefs(RU{ref, zeroOID, pool(i), true})
			}
			return gitPushResult("push", "--porcelain", "origin", pool(i)+":"+ref)
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func expectState(label string, want map[string]string) {
	bad := false
	var parts []string
	for ref, w := range want {
		got := lsRemote(ref)
		parts = append(parts, fmt.Sprintf("%s=%s", filepath.Base(ref), short(got)))
		if got != w {
			bad = true
		}
	}
	sort.Strings(parts)
	verdict := "PASS"
	if bad {
		verdict = "FAIL"
		failed = true
	}
	fmt.Printf("  %-4s %s -> %s\n", verdict, label, strings.Join(parts, " "))
}

func testAtomic() {
	fmt.Println("== T8 atomic multi-ref update")
	c, l := "refs/heads/race-content", "refs/locks/race-doc"
	c1, c2, l1, wrong := pool(31), pool(32), pool(33), pool(34)
	reset := func() { forceSet(c, c1); forceSet(l, l1) }

	reset()
	budget(1)
	ok, d := updateRefs(RU{c, c1, c2, true}, RU{l, wrong, zeroOID, true})
	fmt.Printf("  gql [content ok, lock stale] ok=%v %s\n", ok, trunc(d, 120))
	expectState("gql partial-failure applies nothing", map[string]string{c: c1, l: l1})

	budget(1)
	ok, d = updateRefs(RU{l, l1, zeroOID, true}, RU{c, wrong, c2, true})
	fmt.Printf("  gql [lock ok, content stale] ok=%v %s\n", ok, trunc(d, 120))
	expectState("gql partial-failure (reversed order) applies nothing", map[string]string{c: c1, l: l1})

	budget(1)
	ok, d = updateRefs(RU{c, c1, c2, true}, RU{l, l1, zeroOID, true})
	fmt.Printf("  gql [both valid] ok=%v\n", ok)
	expectState("gql valid batch applies both", map[string]string{c: c2, l: ""})

	reset()
	ok, d = gitPushResult("push", "--porcelain", "--atomic", "--force-with-lease="+c+":"+c1, "--force-with-lease="+l+":"+wrong, "origin", c2+":"+c, ":"+l)
	fmt.Printf("  git --atomic [content ok, lock stale] ok=%v %s\n", ok, trunc(d, 160))
	expectState("git atomic partial-failure applies nothing", map[string]string{c: c1, l: l1})

	ok, d = gitPushResult("push", "--porcelain", "--atomic", "--force-with-lease="+c+":"+c1, "--force-with-lease="+l+":"+l1, "origin", c2+":"+c, ":"+l)
	fmt.Printf("  git --atomic [both valid] ok=%v\n", ok)
	expectState("git atomic valid batch applies both", map[string]string{c: c2, l: ""})
	forceDel(c)
	forceDel(l)
}

func testFencing() {
	fmt.Println("== T9 fencing: stale holder cannot write content")
	c, l := "refs/heads/race-content", "refs/locks/race-doc"
	c1, h, tk, cH, cT := pool(31), pool(35), pool(36), pool(37), pool(38)

	forceSet(c, c1)
	forceDel(l)
	budget(4)
	ok, _ := restCreate(l, h)
	fmt.Printf("  H acquires lock: ok=%v\n", ok)
	ok, _ = updateRefs(RU{l, h, tk, true})
	fmt.Printf("  T takes over expired lease: ok=%v\n", ok)
	ok, d := updateRefs(RU{c, c1, cH, true}, RU{l, h, zeroOID, true})
	fmt.Printf("  H checkin (content+release, fenced by lock=H): ok=%v %s\n", ok, trunc(d, 100))
	expectState("stale holder H rejected, content untouched", map[string]string{c: c1, l: tk})
	ok, _ = updateRefs(RU{c, c1, cT, true}, RU{l, tk, zeroOID, true})
	fmt.Printf("  T checkin: ok=%v\n", ok)
	expectState("current holder T commits and releases", map[string]string{c: cT, l: ""})

	fmt.Println("  -- race: H checkin vs T takeover, 10 rounds")
	t := newTally("T9b fencing race")
	for range 10 {
		forceSet(c, c1)
		forceSet(l, h)
		warm(2)
		budget(2)
		res := race(2, func(i int) (bool, string) {
			if i == 0 {
				return updateRefs(RU{c, c1, cH, true}, RU{l, h, zeroOID, true})
			}
			return updateRefs(RU{l, h, tk, true})
		})
		// Invariant: exactly one wins; content==cH iff H won; lock absent iff H won.
		t.check(res, func(w int) string {
			if w == 0 {
				return ""
			}
			return tk
		}, l)
		wantC := c1
		if res[0].ok {
			wantC = cH
		}
		if got := lsRemote(c); got != wantC {
			fmt.Printf("    ANOMALY content=%s want=%s\n", short(got), short(wantC))
			failed = true
		}
	}
	t.report()
	forceDel(c)
	forceDel(l)
}

func pct(d []time.Duration, p float64) time.Duration {
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[min(len(d)-1, int(float64(len(d))*p))].Round(time.Millisecond)
}

func testConsistency() {
	fmt.Println("== T10 read-after-write consistency + T12 latency")
	ref := "refs/locks/race-cons"
	forceSet(ref, pool(0))
	cur := pool(0)
	var stale = map[string]int{}
	lat := map[string][]time.Duration{}
	timeIt := func(k string, f func() string) string {
		s := time.Now()
		v := f()
		lat[k] = append(lat[k], time.Since(s))
		return v
	}
	const iters = 30
	for i := 1; i <= iters; i++ {
		next := pool(i % 30)
		if next == cur {
			next = pool((i + 1) % 30)
		}
		budget(1)
		var ok bool
		var d string
		timeIt("gql updateRefs", func() string { ok, d = updateRefs(RU{ref, cur, next, true}); return "" })
		if !ok {
			fmt.Printf("  iter %d update failed: %s\n", i, d)
			failed = true
			continue
		}
		cur = next
		if v := timeIt("rest GET ref", func() string { return restGet(ref) }); v != cur {
			stale["rest GET"]++
		}
		if v := timeIt("git ls-remote", func() string { return lsRemote(ref) }); v != cur {
			stale["ls-remote"]++
		}
	}
	// delete visibility
	budget(1)
	ok, _ := updateRefs(RU{ref, cur, zeroOID, true})
	fmt.Printf("  delete ok=%v; immediately: rest=%s ls-remote=%s\n", ok, short(restGet(ref)), short(lsRemote(ref)))
	fmt.Printf("  stale reads after successful write (of %d): rest=%d ls-remote=%d\n", iters, stale["rest GET"], stale["ls-remote"])
	if len(stale) > 0 {
		failed = true
	}

	// extra latency samples
	for range 10 {
		timeIt("git push (lease)", func() string {
			n := pool(len(lat["git push (lease)"]) % 30)
			_, _ = git("push", "--force", "origin", n+":"+ref)
			return ""
		})
	}
	budget(5)
	for i := range 5 {
		timeIt("rest create commit", func() string {
			api("POST", fmt.Sprintf("/repos/%s/%s/git/commits", owner, repo), map[string]any{"tree": st.Tree, "message": fmt.Sprintf(`{"holder":"latency-%d","t":"%s"}`, i, time.Now().Format(time.RFC3339Nano))})
			return ""
		})
	}
	forceDel(ref)
	keys := make([]string, 0, len(lat))
	for k := range lat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  latency %-20s n=%2d p50=%s p95=%s max=%s\n", k, len(lat[k]), pct(lat[k], .5), pct(lat[k], .95), pct(lat[k], 1))
	}
}

func testErrors() {
	fmt.Println("== T11 error shapes")
	ref := "refs/locks/race-err"
	forceSet(ref, pool(0))
	budget(7)
	_, d := updateRefs(RU{ref, pool(1), pool(2), true})
	fmt.Println("  gql wrong beforeOid:      ", d)
	_, d = updateRefs(RU{ref, zeroOID, pool(2), true})
	fmt.Println("  gql create when exists:   ", d)
	_, d = updateRefs(RU{ref, pool(0), "1111111111111111111111111111111111111111", true})
	fmt.Println("  gql afterOid missing obj: ", d)
	_, d = updateRefs(RU{"refs/locks/bad..name", zeroOID, pool(2), true})
	fmt.Println("  gql invalid ref name:     ", d)
	_, d = restCreate(ref, pool(2))
	fmt.Println("  rest create when exists:  ", d)
	_, d = restCreate("refs/locks/race-err2", "1111111111111111111111111111111111111111")
	fmt.Println("  rest create missing obj:  ", d)
	code, b := api("PATCH", fmt.Sprintf("/repos/%s/%s/git/refs/locks/race-err", owner, repo), map[string]any{"sha": pool(5), "force": false})
	fmt.Printf("  rest PATCH force=false non-ff on refs/locks: %d %s\n", code, trunc(string(b), 80))
	_, d = gitPushResult("push", "--porcelain", "--force-with-lease="+ref+":"+pool(9), "origin", pool(3)+":"+ref)
	fmt.Println("  git lease mismatch:       ", trunc(d, 200))
	forceDel(ref)
	forceDel("refs/locks/race-err2")
}

func setup() {
	out, err := exec.Command("gh", "auth", "token").Output()
	must(err)
	token = strings.TrimSpace(string(out))
	if len(st.Pool) == poolN {
		return
	}
	_, err = git("init", "-q")
	must(err)
	_, _ = git("remote", "add", "origin", fmt.Sprintf("git@github.com:%s/%s.git", owner, repo))
	_, err = git("fetch", "-q", "origin", "main")
	must(err)
	st.Tree, err = git("rev-parse", "FETCH_HEAD^{tree}")
	must(err)
	var specs []string
	for i := range poolN {
		sha, err := git("-c", "user.name=lockrace", "-c", "user.email=lockrace@invalid", "commit-tree", st.Tree, "-m", fmt.Sprintf(`{"holder":"racer-%02d","expires":"2026-11-06T12:00:00Z"}`, i))
		must(err)
		st.Pool = append(st.Pool, sha)
		specs = append(specs, fmt.Sprintf("%s:refs/test/pool/%02d", sha, i))
	}
	out2, err := git(append([]string{"push", "-q", "origin"}, specs...)...)
	if err != nil {
		must(fmt.Errorf("pool push: %s", out2))
	}
	_, b := gql(`query{repository(owner:"`+owner+`",name:"`+repo+`"){id}}`, nil)
	var r struct {
		Data struct{ Repository struct{ ID string } } `json:"data"`
	}
	must(json.Unmarshal(b, &r))
	st.RepoID = r.Data.Repository.ID
	saveState()
	fmt.Printf("setup: pool of %d commits pushed under refs/test/pool/*, repoID=%s\n", poolN, st.RepoID)
}

func cleanup() {
	out, _ := git("ls-remote", "origin")
	var dels []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 2 && (strings.HasPrefix(f[1], "refs/test/") || strings.HasPrefix(f[1], "refs/locks/") || f[1] == "refs/heads/race-content") {
			dels = append(dels, ":"+f[1])
		}
	}
	if len(dels) > 0 {
		o, err := git(append([]string{"push", "origin"}, dels...)...)
		fmt.Println("cleanup:", len(dels), "refs deleted", err, trunc(o, 100))
	}
	out, _ = git("ls-remote", "origin")
	fmt.Println("remaining refs:\n" + out)
}

func main() {
	base, _ = os.Getwd()
	gitDir = filepath.Join(base, "repo")
	stPath = filepath.Join(base, "state.json")
	must(os.MkdirAll(gitDir, 0o755))
	gitSSH = "ssh"
	loadState()
	setup()
	tests := map[string]func(){
		"t1": testRestCreate, "t2": testGqlCAS, "t3": testGqlCreate, "t4": testReleaseVsTakeover,
		"t5": testGitLease, "t6": testGitCreate, "t7": testMixed, "t8": testAtomic,
		"t9": testFencing, "t10": testConsistency, "t11": testErrors, "cleanup": cleanup,
		"t13": testAmbiguity, "t14": testContention, "t15": testSideEffects,
	}
	for _, a := range os.Args[1:] {
		f, ok := tests[a]
		if !ok {
			must(fmt.Errorf("unknown test %q", a))
		}
		f()
		fmt.Println()
	}
	if failed {
		fmt.Println("RESULT: ANOMALIES DETECTED")
		os.Exit(1)
	}
	fmt.Println("RESULT: all checks passed")
}
