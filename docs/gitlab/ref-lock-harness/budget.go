package main

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// GitLab Free: 100/min API and 100/min Git HTTPS+LFS per user. One conservative
// combined window keeps the harness well under both.
const perMin = 80

var (
	bmu    sync.Mutex
	window []time.Time
	inRace atomic.Bool // racers use a reservation taken before the gate opens
	used   atomic.Int64
)

func take(n int) {
	for {
		bmu.Lock()
		cut := time.Now().Add(-time.Minute)
		k := 0
		for _, t := range window {
			if t.After(cut) {
				window[k] = t
				k++
			}
		}
		window = window[:k]
		if len(window)+n <= perMin {
			for range n {
				window = append(window, time.Now())
			}
			bmu.Unlock()
			used.Add(int64(n))
			return
		}
		wait := time.Until(window[0].Add(time.Minute)) + 250*time.Millisecond
		bmu.Unlock()
		fmt.Printf("  (pacing %s for GitLab rate budget)\n", wait.Round(time.Second))
		time.Sleep(wait)
	}
}

func charge() {
	if !inRace.Load() {
		take(1)
	}
}
