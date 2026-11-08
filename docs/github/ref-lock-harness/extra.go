package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// updateRefsTimeout sends updateRefs but abandons the request after d.
func updateRefsTimeout(d time.Duration, us ...RU) (bool, string) {
	b, _ := json.Marshal(map[string]any{"query": updateRefsQ, "variables": map[string]any{"rid": st.RepoID, "u": us}})
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.github.com/graphql", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return false, "CLIENT-ABORT: " + err.Error()
	}
	defer resp.Body.Close()
	var r struct{ Errors []any }
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode == 200 && len(r.Errors) == 0, fmt.Sprint(resp.StatusCode)
}

func testAmbiguity() {
	fmt.Println("== T13 ambiguous outcome: client aborts mid-request, read-back resolves")
	ref := "refs/locks/race-amb"
	cur := pool(0)
	forceSet(ref, cur)
	warm(1)
	var aborted, appliedAfterAbort, notApplied, wrong int
	for i := 1; i <= 12; i++ {
		next := pool(i)
		budget(1)
		timeout := []time.Duration{150, 300, 450}[i%3] * time.Millisecond
		ok, d := updateRefsTimeout(timeout, RU{ref, cur, next, true})
		time.Sleep(1500 * time.Millisecond) // let the server finish
		got := restGet(ref)
		state := "?"
		switch got {
		case next:
			state = "applied"
		case cur:
			state = "not applied"
		default:
			state = "UNEXPECTED " + short(got)
			wrong++
		}
		if strings.HasPrefix(d, "CLIENT-ABORT") {
			aborted++
			if got == next {
				appliedAfterAbort++
			} else if got == cur {
				notApplied++
			}
		} else if ok != (got == next) {
			wrong++
			state += " (MISMATCH with reported ok)"
		}
		fmt.Printf("  i=%2d timeout=%s reported=%-5v %-14s actual=%s\n", i, timeout, ok, trunc(d, 14), state)
		if got == next || got == cur {
			cur = got
		}
	}
	fmt.Printf("  aborted=%d of which applied=%d not-applied=%d; inconsistencies=%d\n", aborted, appliedAfterAbort, notApplied, wrong)
	if wrong > 0 {
		failed = true
	}
	forceDel(ref)
}

func testContention() {
	t := newTally("T14 high contention: 16 racers, REST+GraphQL acquire")
	ref := "refs/locks/race-hi"
	n := 16
	for range 3 {
		forceDel(ref)
		warm(n)
		budget(n)
		res := race(n, func(i int) (bool, string) {
			if i%2 == 0 {
				return restCreate(ref, pool(i))
			}
			return updateRefs(RU{ref, zeroOID, pool(i), true})
		})
		t.check(res, pool, ref)
	}
	forceDel(ref)
	t.report()
}

func testSideEffects() {
	fmt.Println("== T15 side effects")
	_, b := api("GET", fmt.Sprintf("/repos/%s/%s/events?per_page=100", owner, repo), nil)
	var evs []struct {
		Type    string
		Payload struct{ Ref string }
		Created string `json:"created_at"`
	}
	_ = json.Unmarshal(b, &evs)
	counts := map[string]int{}
	for _, e := range evs {
		counts[e.Type+" "+e.Payload.Ref]++
	}
	fmt.Printf("  public repo events (last 100): %d events\n", len(evs))
	for k, v := range counts {
		fmt.Printf("    %-50s x%d\n", k, v)
	}
	budget(1)
	local := time.Now().UTC()
	code, cb := api("POST", fmt.Sprintf("/repos/%s/%s/git/commits", owner, repo), map[string]any{"tree": st.Tree, "message": "clock probe"})
	var c struct {
		Committer struct {
			Date string
			Name string
		}
	}
	_ = json.Unmarshal(cb, &c)
	fmt.Printf("  API commit (%d) committer=%q date=%s; local clock at send=%s\n", code, c.Committer.Name, c.Committer.Date, local.Format(time.RFC3339))
	_, rb := api("GET", "/rate_limit", nil)
	var rl struct {
		Resources map[string]struct{ Limit, Used, Remaining int } `json:"resources"`
	}
	_ = json.Unmarshal(rb, &rl)
	for _, k := range []string{"core", "graphql"} {
		r := rl.Resources[k]
		fmt.Printf("  primary rate limit %-8s used=%d/%d remaining=%d\n", k, r.Used, r.Limit, r.Remaining)
	}
}
