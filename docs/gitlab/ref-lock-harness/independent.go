package main

import (
	"fmt"
	"sort"
)

// testIndependent pushes to n DISTINCT refs at once. There is no logical conflict,
// so every push should succeed; failures are spurious (liveness, not safety).
func testIndependent() {
	fmt.Println("== L10 concurrent pushes to distinct refs (no logical conflict)")
	reasons := map[string]int{}
	total, fails := 0, 0
	for r := range 8 {
		refs := make([]string, racers)
		for i := range refs {
			refs[i] = fmt.Sprintf("refs/locks/indep-%d", i)
			forceDel(refs[i])
		}
		res := race(racers, func(i int) (bool, string) {
			return gitOK("push", "--porcelain", "origin", pool(i)+":"+refs[i])
		})
		nf := 0
		for i, x := range res {
			total++
			if !x.ok {
				nf++
				fails++
				reasons[classify(x.detail)]++
			}
			// read-back resolves every outcome
			got := lsRemote(refs[i])
			if x.ok && got != pool(i) {
				fmt.Printf("    ANOMALY: racer %d reported ok but ref=%s\n", i, short(got))
				failed = true
			}
			if !x.ok && got == pool(i) {
				reasons["(failed report but actually applied)"]++
			}
		}
		fmt.Printf("  round %d: %d/%d pushes failed\n", r+1, nf, racers)
	}
	fmt.Printf("  total spurious failures: %d of %d (%.0f%%)\n", fails, total, 100*float64(fails)/float64(total))
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("   %-60s x%d\n", k, reasons[k])
	}
	for i := range racers {
		forceDel(fmt.Sprintf("refs/locks/indep-%d", i))
	}
}
