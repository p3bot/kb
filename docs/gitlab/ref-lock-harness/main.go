// Command glrace stress-tests GitLab refs (and LFS locks) as a distributed lock primitive.
//
// Environment:
//
//	GL_TOKEN_FILE  path to a file holding a GitLab token (api, write_repository)
//	GIT_ASKPASS    askpass script returning "oauth2" / the token, for git over HTTPS
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	project   = "grantcarthew/ref-test"
	projectID = "88596047"
	remote    = "https://gitlab.com/grantcarthew/ref-test.git"
	poolN     = 40
	rounds    = 8
	racers    = 8
)

var (
	base         string
	gitDir       string
	token        string
	client       = &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 32, MaxConnsPerHost: 64}}
	st           state
	stPath       string
	failed       bool
	hookDeclines int
	setupRetries int
)

type state struct {
	Tree string   `json:"tree"`
	Main string   `json:"main"`
	Pool []string `json:"pool"`
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

func short(s string) string {
	if s == "" {
		return "(absent)"
	}
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// ---------- HTTP ----------

func do(req *http.Request) (int, []byte) {
	charge()
	resp, err := client.Do(req)
	if err != nil {
		return -1, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 429 {
		fmt.Fprintf(os.Stderr, "FATAL: rate limited, retry-after=%s name=%s observed=%s\n", resp.Header.Get("Retry-After"), resp.Header.Get("RateLimit-Name"), resp.Header.Get("RateLimit-Observed"))
		os.Exit(3)
	}
	return resp.StatusCode, b
}

func api(method, path string, form url.Values) (int, []byte) {
	var r io.Reader
	if form != nil {
		r = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, "https://gitlab.com/api/v4/projects/"+projectID+path, r)
	req.Header.Set("PRIVATE-TOKEN", token)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	return do(req)
}

func lfs(method, path string, body any) (int, []byte) {
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, remote+"/info/lfs/locks"+path, bytes.NewReader(b))
	req.SetBasicAuth("oauth2", token)
	req.Header.Set("Accept", "application/vnd.git-lfs+json")
	req.Header.Set("Content-Type", "application/vnd.git-lfs+json")
	return do(req)
}

// restRead resolves any ref (branch or custom namespace) through the commits API.
func restRead(ref string) string {
	code, b := api("GET", "/repository/commits/"+url.PathEscape(ref), nil)
	if code == 404 {
		return ""
	}
	var c struct{ ID string }
	_ = json.Unmarshal(b, &c)
	if c.ID == "" {
		return fmt.Sprintf("ERR:%d %s", code, trunc(string(b), 80))
	}
	return c.ID
}

func branchCreate(name, sha string) (bool, string) {
	code, b := api("POST", "/repository/branches", url.Values{"branch": {name}, "ref": {sha}})
	return code == 201, fmt.Sprintf("%d %s", code, trunc(string(b), 120))
}

// ---------- git ----------

func gitCtx(ctx context.Context, args ...string) (string, error) {
	if len(args) > 0 && (args[0] == "push" || args[0] == "ls-remote" || args[0] == "fetch") {
		charge()
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = gitDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func git(args ...string) (string, error) { return gitCtx(context.Background(), args...) }

func gitOK(args ...string) (bool, string) {
	out, err := git(args...)
	return err == nil, out
}

func lsRemote(ref string) string {
	out, err := git("ls-remote", "origin", ref)
	if err != nil {
		return "ERR:" + trunc(out, 80)
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == ref {
			return f[0]
		}
	}
	return ""
}

// forceSet and forceDel are harness setup, not part of the protocol under test;
// they retry because GitLab's pre-receive hook occasionally declines transiently.
func forceSet(ref, sha string) {
	var out string
	var err error
	for a := range 5 {
		if out, err = git("push", "--force", "origin", sha+":"+ref); err == nil {
			return
		}
		setupRetries++
		fmt.Printf("  (setup push retry %d: %s)\n", a+1, trunc(out, 120))
		time.Sleep(time.Duration(2+a*2) * time.Second)
	}
	must(fmt.Errorf("forceSet %s: %s", ref, out))
}

func forceDel(ref string) {
	for a := range 5 {
		out, err := git("push", "origin", ":"+ref)
		if err == nil || strings.Contains(out, "unable to delete") || !strings.Contains(out, "pre-receive") {
			return
		}
		setupRetries++
		fmt.Printf("  (setup delete retry %d)\n", a+1)
		time.Sleep(time.Duration(2+a*2) * time.Second)
	}
}

func pool(i int) string { return st.Pool[i] }

// ---------- race machinery ----------

type result struct {
	ok         bool
	start, end time.Time
	detail     string
}

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
	take(n)
	inRace.Store(true)
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	inRace.Store(false)
	return res
}

// warm opens one keep-alive connection; GitLab Free's 100/min budget is too
// tight to pre-open a connection per racer.
func warm(int) { api("GET", "", nil) }

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

func classify(d string) string {
	switch {
	case strings.Contains(d, "pre-receive hook declined"):
		hookDeclines++
		return "git: SERVER pre-receive hook declined (transient?)"
	case strings.Contains(d, "remote rejected"):
		reason := "?"
		if i := strings.Index(d, "remote rejected] ("); i >= 0 {
			reason = d[i+len("remote rejected] ("):]
		} else if i := strings.Index(d, "[remote rejected]"); i >= 0 {
			reason = d[i+len("[remote rejected]"):]
		}
		if j := strings.IndexAny(reason, ")|"); j >= 0 {
			reason = reason[:j]
		}
		return "git: SERVER-side reject (" + strings.TrimSpace(reason) + ")"
	case strings.Contains(d, "stale info"):
		return "git: client-side lease reject (stale info)"
	case strings.Contains(d, "non-fast-forward"), strings.Contains(d, "fetch first"):
		return "git: client-side reject (exists)"
	case strings.Contains(d, "Branch already exists"):
		return "api: 400 Branch already exists"
	case strings.Contains(d, "invalid reference name"):
		return "api: 400 invalid reference name (concurrent loser)"
	case strings.Contains(d, "already locked"):
		return "lfs: 409 already locked"
	case strings.Contains(d, "failed to push"):
		return "git: other reject"
	}
	return "other: " + trunc(d, 70)
}

// check: exactly one winner; ls-remote and REST agree with expect(winner).
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
		minS, maxS, minE = minT(minS, r.start), maxT(maxS, r.start), minT(minE, r.end)
	}
	spread, overlap := maxS.Sub(minS), minE.Sub(maxS)
	t.maxSpread = max(t.maxSpread, spread)
	t.minOverlap = min(t.minOverlap, overlap)
	if overlap <= 0 {
		t.allOverlap = false
	}
	status := "ok"
	var ls, rr string
	if ref != "" {
		ls, rr = lsRemote(ref), restRead(ref)
	}
	if len(winners) != 1 {
		status = fmt.Sprintf("ANOMALY: %d winners %v", len(winners), winners)
	} else if ref != "" {
		if want := expect(winners[0]); ls != want || rr != want {
			status = fmt.Sprintf("ANOMALY: winner=%d want=%s ls-remote=%s rest=%s", winners[0], short(want), short(ls), short(rr))
		}
	}
	if status == "ok" {
		t.good++
	} else {
		t.anomaly++
		failed = true
		for i, r := range res {
			fmt.Printf("    racer %d ok=%v %s\n", i, r.ok, trunc(r.detail, 200))
		}
	}
	fmt.Printf("  round %2d: winners=%v spread=%s overlap=%s final=%s %s\n", t.rounds, winners,
		spread.Round(time.Millisecond), overlap.Round(time.Millisecond), short(ls), status)
}

func minT(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxT(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
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
		fmt.Printf("   loser outcome %-44s x%d\n", k, t.notes[k])
	}
}

// ---------- tests ----------

func testGitCreate() {
	t := newTally("L1 git push create race (refs/locks)")
	ref := "refs/locks/race-gitc"
	for range rounds {
		forceDel(ref)
		res := race(racers, func(i int) (bool, string) { return gitOK("push", "--porcelain", "origin", pool(i)+":"+ref) })
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testGitLease() {
	t := newTally("L2 git push --force-with-lease race")
	ref := "refs/locks/race-git"
	b := pool(30)
	for range rounds {
		forceSet(ref, b)
		res := race(racers, func(i int) (bool, string) {
			return gitOK("push", "--porcelain", "--force-with-lease="+ref+":"+b, "origin", pool(i)+":"+ref)
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testGitReleaseVsTakeover() {
	t := newTally("L3 git release (delete) vs takeover race")
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
		res := race(racers, func(i int) (bool, string) {
			if i%2 == 0 {
				return gitOK("push", "--porcelain", "--force-with-lease="+ref+":"+b, "origin", ":"+ref)
			}
			return gitOK("push", "--porcelain", "--force-with-lease="+ref+":"+b, "origin", pool(i)+":"+ref)
		})
		t.check(res, expect, ref)
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
	v := "PASS"
	if bad {
		v, failed = "FAIL", true
	}
	fmt.Printf("  %-4s %s -> %s\n", v, label, strings.Join(parts, " "))
}

func testFencing() {
	fmt.Println("== L4 fencing with git push --atomic")
	c, l := "refs/heads/race-content", "refs/locks/race-doc"
	c1, h, tk, cH, cT := pool(31), pool(35), pool(36), pool(37), pool(38)
	forceSet(c, c1)
	forceDel(l)
	ok, _ := gitOK("push", "--porcelain", "origin", h+":"+l)
	fmt.Printf("  H acquires: ok=%v\n", ok)
	ok, _ = gitOK("push", "--porcelain", "--force-with-lease="+l+":"+h, "origin", tk+":"+l)
	fmt.Printf("  T takes over: ok=%v\n", ok)
	ok, d := gitOK("push", "--porcelain", "--atomic", "--force-with-lease="+c+":"+c1, "--force-with-lease="+l+":"+h, "origin", "+"+cH+":"+c, ":"+l)
	fmt.Printf("  H atomic checkin: ok=%v %s\n", ok, trunc(d, 140))
	expectState("stale holder rejected, content untouched", map[string]string{c: c1, l: tk})
	ok, _ = gitOK("push", "--porcelain", "--atomic", "--force-with-lease="+c+":"+c1, "--force-with-lease="+l+":"+tk, "origin", "+"+cT+":"+c, ":"+l)
	fmt.Printf("  T atomic checkin: ok=%v\n", ok)
	expectState("current holder commits and releases", map[string]string{c: cT, l: ""})

	fmt.Println("  -- race: H atomic checkin vs T takeover, 10 rounds")
	t := newTally("L4b fencing race (git)")
	for range 10 {
		forceSet(c, c1)
		forceSet(l, h)
		res := race(2, func(i int) (bool, string) {
			if i == 0 {
				return gitOK("push", "--porcelain", "--atomic", "--force-with-lease="+c+":"+c1, "--force-with-lease="+l+":"+h, "origin", "+"+cH+":"+c, ":"+l)
			}
			return gitOK("push", "--porcelain", "--force-with-lease="+l+":"+h, "origin", tk+":"+l)
		})
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

func testBranchAPI() {
	t := newTally("L5 API branch-create race (refs/heads/locks/*)")
	name := "locks/race-api"
	ref := "refs/heads/" + name
	for range 6 {
		forceDel(ref)
		warm(racers)
		res := race(racers, func(i int) (bool, string) { return branchCreate(name, pool(i)) })
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testMixed() {
	t := newTally("L6 mixed API branch-create vs git push create race")
	name := "locks/race-mix"
	ref := "refs/heads/" + name
	for range rounds / 2 {
		forceDel(ref)
		warm(racers)
		res := race(racers, func(i int) (bool, string) {
			if i%2 == 0 {
				return branchCreate(name, pool(i))
			}
			return gitOK("push", "--porcelain", "origin", pool(i)+":"+ref)
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testLFS() {
	t := newTally("L7 LFS lock create race")
	for r := range 6 {
		path := fmt.Sprintf("docs/race-%d-%d.md", time.Now().Unix(), r)
		ids := make([]string, racers)
		res := race(racers, func(i int) (bool, string) {
			code, b := lfs("POST", "", map[string]string{"path": path})
			var l struct{ Lock struct{ ID string } }
			_ = json.Unmarshal(b, &l)
			ids[i] = l.Lock.ID
			return code == 201, fmt.Sprintf("%d %s", code, trunc(string(b), 400))
		})
		t.check(res, nil, "")
		// all racers must report the same lock id (winner's), losers see it via 409 body
		uniq := map[string]bool{}
		for _, id := range ids {
			uniq[id] = true
		}
		if len(uniq) != 1 {
			fmt.Printf("    ANOMALY: lock ids seen %v\n", uniq)
			failed = true
		}
		for id := range uniq {
			code, _ := lfs("POST", "/"+id+"/unlock", map[string]any{})
			if code != 200 {
				fmt.Printf("    unlock %s -> %d\n", id, code)
			}
		}
	}
	t.report()
}

func testConsistency() {
	fmt.Println("== L8 read-after-write consistency + latency")
	ref := "refs/locks/race-cons"
	cur := pool(0)
	forceSet(ref, cur)
	lat := map[string][]time.Duration{}
	tm := func(k string, f func() string) string {
		s := time.Now()
		v := f()
		lat[k] = append(lat[k], time.Since(s))
		return v
	}
	stale := map[string]int{}
	const iters = 30
	for i := 1; i <= iters; i++ {
		next := pool(i % 30)
		if next == cur {
			next = pool((i + 1) % 30)
		}
		var ok bool
		tm("git push lease (https)", func() string {
			ok, _ = gitOK("push", "--porcelain", "--force-with-lease="+ref+":"+cur, "origin", next+":"+ref)
			return ""
		})
		if !ok {
			fmt.Printf("  iter %d push failed\n", i)
			failed = true
			continue
		}
		cur = next
		if tm("rest read ref", func() string { return restRead(ref) }) != cur {
			stale["rest"]++
		}
		if tm("git ls-remote (https)", func() string { return lsRemote(ref) }) != cur {
			stale["ls-remote"]++
		}
	}
	_, _ = gitOK("push", "--porcelain", "--force-with-lease="+ref+":"+cur, "origin", ":"+ref)
	fmt.Printf("  delete; immediately rest=%s ls-remote=%s\n", short(restRead(ref)), short(lsRemote(ref)))
	fmt.Printf("  stale reads after successful write (of %d): rest=%d ls-remote=%d\n", iters, stale["rest"], stale["ls-remote"])
	if len(stale) > 0 {
		failed = true
	}
	for i := range 8 {
		name := fmt.Sprintf("locks/lat-%d", i)
		tm("api branch create", func() string { branchCreate(name, pool(i)); return "" })
		tm("api branch delete", func() string { api("DELETE", "/repository/branches/"+url.PathEscape(name), nil); return "" })
	}
	for i := range 8 {
		var id string
		tm("lfs lock create", func() string {
			_, b := lfs("POST", "", map[string]string{"path": fmt.Sprintf("docs/lat-%d-%d.md", time.Now().UnixNano(), i)})
			var l struct{ Lock struct{ ID string } }
			_ = json.Unmarshal(b, &l)
			id = l.Lock.ID
			return ""
		})
		tm("lfs unlock", func() string { lfs("POST", "/"+id+"/unlock", map[string]any{}); return "" })
	}
	keys := make([]string, 0, len(lat))
	for k := range lat {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := lat[k]
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		p := func(q float64) time.Duration { return d[min(len(d)-1, int(float64(len(d))*q))].Round(time.Millisecond) }
		fmt.Printf("  latency %-24s n=%2d p50=%s p95=%s max=%s\n", k, len(d), p(.5), p(.95), p(1))
	}
}

func testAmbiguity() {
	fmt.Println("== L9 ambiguous outcome: git push killed mid-flight, read-back resolves")
	ref := "refs/locks/race-amb"
	cur := pool(0)
	forceSet(ref, cur)
	var aborted, applied, notApplied, wrong int
	for i := 1; i <= 18; i++ {
		next := pool(i)
		d := []time.Duration{1000, 1150, 1300, 1450, 1600, 1750}[i%6] * time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), d)
		_, err := gitCtx(ctx, "push", "--porcelain", "--force-with-lease="+ref+":"+cur, "origin", next+":"+ref)
		killed := ctx.Err() != nil
		cancel()
		time.Sleep(1500 * time.Millisecond)
		got := restRead(ref)
		state := "UNEXPECTED " + short(got)
		switch got {
		case next:
			state = "applied"
		case cur:
			state = "not applied"
		default:
			wrong++
		}
		if killed {
			aborted++
			if got == next {
				applied++
			} else if got == cur {
				notApplied++
			}
		} else if (err == nil) != (got == next) {
			wrong++
			state += " (MISMATCH with exit status)"
		}
		fmt.Printf("  i=%2d kill-after=%s killed=%-5v err=%-5v actual=%s\n", i, d, killed, err != nil, state)
		if got == next || got == cur {
			cur = got
		}
	}
	fmt.Printf("  killed=%d of which applied=%d not-applied=%d; inconsistencies=%d\n", aborted, applied, notApplied, wrong)
	if wrong > 0 {
		failed = true
	}
	forceDel(ref)
}

// ---------- setup / cleanup ----------

func setup() {
	b, err := os.ReadFile(os.Getenv("GL_TOKEN_FILE"))
	must(err)
	token = strings.TrimSpace(string(b))
	if s, err := os.ReadFile(stPath); err == nil {
		must(json.Unmarshal(s, &st))
	}
	if len(st.Pool) == poolN {
		return
	}
	_, err = git("init", "-q")
	must(err)
	_, _ = git("remote", "add", "origin", remote)
	if out, err := git("fetch", "-q", "origin", "main"); err != nil {
		must(fmt.Errorf("fetch: %s", out))
	}
	st.Tree, _ = git("rev-parse", "FETCH_HEAD^{tree}")
	st.Main, _ = git("rev-parse", "FETCH_HEAD")
	var specs []string
	for i := range poolN {
		sha, err := git("-c", "user.name=lockrace", "-c", "user.email=lockrace@invalid", "commit-tree", st.Tree, "-m",
			fmt.Sprintf(`{"holder":"racer-%02d","expires":"2026-11-07T00:00:00Z"}`, i))
		must(err)
		st.Pool = append(st.Pool, sha)
		specs = append(specs, fmt.Sprintf("%s:refs/test/pool/%02d", sha, i))
	}
	if out, err := git(append([]string{"push", "-q", "origin"}, specs...)...); err != nil {
		must(fmt.Errorf("pool push: %s", out))
	}
	s, _ := json.MarshalIndent(st, "", " ")
	must(os.WriteFile(stPath, s, 0o600))
	fmt.Printf("setup: %d pool commits pushed under refs/test/pool/*\n", poolN)
}

func cleanup() {
	out, _ := git("ls-remote", "origin")
	var dels []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) != 2 {
			continue
		}
		r := f[1]
		if strings.HasPrefix(r, "refs/test/") || strings.HasPrefix(r, "refs/locks/") || strings.HasPrefix(r, "refs/p3bot/") ||
			strings.HasPrefix(r, "refs/heads/locks/") || r == "refs/heads/race-content" {
			dels = append(dels, ":"+r)
		}
	}
	if len(dels) > 0 {
		o, err := git(append([]string{"push", "origin"}, dels...)...)
		fmt.Println("cleanup:", len(dels), "refs deleted, err:", err, trunc(o, 80))
	}
	code, b := lfs("POST", "/verify", map[string]any{})
	var v struct{ Ours, Theirs []struct{ ID, Path string } }
	_ = json.Unmarshal(b, &v)
	for _, l := range append(v.Ours, v.Theirs...) {
		c, _ := lfs("POST", "/"+l.ID+"/unlock", map[string]any{"force": true})
		fmt.Printf("cleanup: unlock LFS %s %s -> %d\n", l.ID, l.Path, c)
	}
	_ = code
	out, _ = git("ls-remote", "origin")
	fmt.Println("remaining refs:\n" + out)
}

func main() {
	base, _ = os.Getwd()
	gitDir = filepath.Join(base, "repo")
	stPath = filepath.Join(base, "state.json")
	must(os.MkdirAll(gitDir, 0o755))
	setup()
	tests := map[string]func(){
		"l1": testGitCreate, "l2": testGitLease, "l3": testGitReleaseVsTakeover, "l4": testFencing,
		"l5": testBranchAPI, "l6": testMixed, "l7": testLFS, "l8": testConsistency, "l9": testAmbiguity, "l10": testIndependent,
		"cleanup": cleanup,
	}
	for _, a := range os.Args[1:] {
		f, ok := tests[a]
		if !ok {
			must(fmt.Errorf("unknown test %q", a))
		}
		f()
		fmt.Println()
	}
	fmt.Printf("requests charged against budget: %d\n", used.Load())
	fmt.Printf("hook declines seen by racers: %d; setup push retries: %d\n", hookDeclines, setupRetries)
	if failed {
		fmt.Println("RESULT: ANOMALIES DETECTED")
		os.Exit(1)
	}
	fmt.Println("RESULT: all checks passed")
}
