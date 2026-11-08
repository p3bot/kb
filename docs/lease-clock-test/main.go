// Command leaseclock checks whether a host's HTTP Date header is a sound lease clock
// for lock commits created locally and pushed over git transport.
//
// Usage: leaseclock github|gitlab|cleanup-github|cleanup-gitlab
//
// GitHub: token from GITHUB_TOKEN, repo grantcarthew/pentest.
// GitLab: token from the file named by GL_TOKEN_FILE, project grantcarthew/ref-test.
// Tokens reach git through a credential helper, never the command line.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	iterations = 20
	lockRef    = "refs/locks/date-test"
	glBranch   = "date-test"
)

type host struct {
	name    string
	remote  string
	user    string
	apiBase string
	authHdr func(*http.Request)
	refPath string // REST read used for t0/t1
	stamp   func(i int, tree string) (stamp, hdr time.Time, err error)
	cleanup func()
}

var (
	token        string
	gitDir       string
	client       = &http.Client{Timeout: 60 * time.Second}
	h            host
	cleaning     bool
	glBranchMade bool
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: leaseclock github|gitlab|cleanup-github|cleanup-gitlab")
		os.Exit(2)
	}
	mode := os.Args[1]
	switch strings.TrimPrefix(mode, "cleanup-") {
	case "github":
		token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		h = github()
	case "gitlab":
		b, err := os.ReadFile(os.Getenv("GL_TOKEN_FILE"))
		must(err)
		token = strings.TrimSpace(string(b))
		h = gitlab()
	default:
		os.Exit(2)
	}
	if token == "" {
		must(fmt.Errorf("no token"))
	}
	dir, err := os.MkdirTemp("", "datetest-")
	must(err)
	defer os.RemoveAll(dir)
	gitDir = dir
	gitMust("init", "-q")
	gitMust("remote", "add", "origin", h.remote)
	gitMust("fetch", "-q", "origin", "main")
	if strings.HasPrefix(mode, "cleanup-") {
		h.cleanup()
		return
	}
	run()
	h.cleanup()
}

func run() {
	tree := gitMust("rev-parse", "FETCH_HEAD^{tree}")
	prev := ""
	var maxStampSkew, maxLocalSkew, maxBracket, maxPush time.Duration
	var lastDate time.Time
	backwards, stampFail := 0, 0
	fmt.Printf("%s: %d iterations\n", h.name, iterations)
	fmt.Println(" i  t0(Date)             push    t1-t0  stamp-hdr  local-Date")
	for i := range iterations {
		t0, local0, err := dateOf("GET", h.refPath, nil)
		must(err)
		msg := fmt.Sprintf(`{"holder":"datetest","start":%q,"ttl":"10m"}`, t0.UTC().Format(time.RFC3339))
		sha := gitMust("-c", "user.name=datetest", "-c", "user.email=datetest@invalid", "commit-tree", tree, "-m", msg)
		ps := time.Now()
		out, err := git("push", "--porcelain", "--force-with-lease="+lockRef+":"+prev, "origin", sha+":"+lockRef)
		push := time.Since(ps)
		if err != nil {
			must(fmt.Errorf("push %d: %s", i, out))
		}
		prev = sha
		t1, _, err := dateOf("GET", h.refPath, nil)
		must(err)
		var stamp, hdr time.Time
		for a := range 4 {
			if stamp, hdr, err = h.stamp(i*10+a, tree); err == nil {
				break
			}
			fmt.Printf("    stamp retry %d: %s\n", a+1, trunc(err.Error()))
			time.Sleep(time.Duration(a+1) * 2 * time.Second)
		}
		must(err)

		sk := stamp.Sub(hdr)
		if abs(sk) > time.Second {
			stampFail++
		}
		lsk := local0.Sub(t0)
		br := t1.Sub(t0)
		for _, d := range []time.Time{t0, t1, hdr} {
			if !lastDate.IsZero() && d.Before(lastDate) {
				backwards++
			}
			lastDate = d
		}
		maxStampSkew = max(maxStampSkew, abs(sk))
		maxLocalSkew = max(maxLocalSkew, abs(lsk))
		maxBracket = max(maxBracket, br)
		maxPush = max(maxPush, push)
		fmt.Printf("%2d  %s  %5.2fs  %4.0fs  %+6.1fs    %+6.2fs\n", i, t0.UTC().Format(time.RFC3339), push.Seconds(), br.Seconds(), sk.Seconds(), lsk.Seconds())
	}
	if got := lsRemote(lockRef); got != prev {
		fmt.Printf("read-back mismatch: ls-remote %q want %q\n", got, prev)
		stampFail++
	}
	fmt.Printf("max |stamp - Date| %.1fs (fails >1s: %d); max t1-t0 %.0fs; max push %.2fs; max |local - Date| %.2fs; Date went backwards: %d\n",
		maxStampSkew.Seconds(), stampFail, maxBracket.Seconds(), maxPush.Seconds(), maxLocalSkew.Seconds(), backwards)
	if stampFail > 0 || backwards > 0 {
		os.Exit(1)
	}
}

// ---------- hosts ----------

func github() host {
	base := "https://api.github.com/repos/grantcarthew/pentest"
	return host{
		name:    "github",
		remote:  "https://github.com/grantcarthew/pentest.git",
		user:    "x-access-token",
		apiBase: base,
		authHdr: func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer "+token)
			r.Header.Set("Accept", "application/vnd.github+json")
			r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		},
		refPath: base + "/git/ref/heads/main",
		stamp: func(i int, tree string) (time.Time, time.Time, error) {
			body := map[string]any{"message": fmt.Sprintf("datetest stamp %d", i), "tree": tree}
			var resp struct {
				Committer struct{ Date string } `json:"committer"`
			}
			hdr, err := apiJSON("POST", base+"/git/commits", body, &resp)
			if err != nil {
				return time.Time{}, time.Time{}, err
			}
			s, err := time.Parse(time.RFC3339, resp.Committer.Date)
			return s, hdr, err
		},
		cleanup: func() {
			if sha := lsRemote(lockRef); sha != "" {
				out, err := git("push", "--force-with-lease="+lockRef+":"+sha, "origin", ":"+lockRef)
				fmt.Printf("cleanup %s: err=%v %s\n", lockRef, err, trunc(out))
			}
		},
	}
}

func gitlab() host {
	base := "https://gitlab.com/api/v4/projects/88596047"
	return host{
		name:    "gitlab",
		remote:  "https://gitlab.com/grantcarthew/ref-test.git",
		user:    "oauth2",
		apiBase: base,
		authHdr: func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", token) },
		refPath: base + "/repository/branches/main",
		stamp: func(i int, _ string) (time.Time, time.Time, error) {
			body := map[string]any{
				"branch":         glBranch,
				"commit_message": fmt.Sprintf("datetest stamp %d", i),
				"actions": []map[string]any{{
					"action": "create", "file_path": fmt.Sprintf("datetest/%03d.txt", i), "content": "x",
				}},
			}
			if !glBranchMade {
				body["start_branch"] = "main"
			}
			var resp struct {
				CommittedDate string `json:"committed_date"`
			}
			hdr, err := apiJSON("POST", base+"/repository/commits", body, &resp)
			if err != nil {
				return time.Time{}, time.Time{}, err
			}
			glBranchMade = true
			s, err := time.Parse(time.RFC3339, resp.CommittedDate)
			return s, hdr, err
		},
		cleanup: func() {
			if sha := lsRemote(lockRef); sha != "" {
				out, err := git("push", "--force-with-lease="+lockRef+":"+sha, "origin", ":"+lockRef)
				fmt.Printf("cleanup %s: err=%v %s\n", lockRef, err, trunc(out))
			}
			hdr, err := apiJSON("DELETE", base+"/repository/branches/"+url.PathEscape(glBranch), nil, nil)
			fmt.Printf("cleanup branch %s: err=%v (Date %s)\n", glBranch, err, hdr.UTC().Format(time.RFC3339))
		},
	}
}

// ---------- http ----------

func dateOf(method, u string, body any) (time.Time, time.Time, error) {
	hdr, err := apiJSON(method, u, body, nil)
	return hdr, time.Now(), err
}

func apiJSON(method, u string, body, out any) (time.Time, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, rd)
	h.authHdr(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return time.Time{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	d, derr := http.ParseTime(resp.Header.Get("Date"))
	if resp.StatusCode >= 300 {
		return d, fmt.Errorf("%s %s: %d %s", method, u, resp.StatusCode, trunc(string(b)))
	}
	if derr != nil {
		return d, fmt.Errorf("no Date header: %v", derr)
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return d, err
		}
	}
	return d, nil
}

// ---------- git ----------

func git(args ...string) (string, error) {
	helper := "!f() { echo username=" + h.user + "; echo \"password=$DATETEST_TOKEN\"; }; f"
	u, _ := url.Parse(h.remote)
	scoped := "credential." + u.Scheme + "://" + u.Host + ".helper"
	full := append([]string{
		"-c", "credential.helper=", "-c", scoped + "=", "-c", scoped + "=" + helper,
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = gitDir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "DATETEST_TOKEN="+token)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func gitMust(args ...string) string {
	out, err := git(args...)
	if err != nil {
		must(fmt.Errorf("git %s: %s", args[0], out))
	}
	return out
}

func lsRemote(ref string) string {
	out, err := git("ls-remote", "origin", ref)
	if err != nil {
		must(fmt.Errorf("ls-remote: %s", out))
	}
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[1] == ref {
			return f[0]
		}
	}
	return ""
}

// ---------- util ----------

func abs(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func trunc(s string) string {
	s = strings.ReplaceAll(s, token, "<token>")
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", trunc(err.Error()))
		if h.cleanup != nil && gitDir != "" && !cleaning {
			cleaning = true
			h.cleanup()
		}
		os.Exit(2)
	}
}
