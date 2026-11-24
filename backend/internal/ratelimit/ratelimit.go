// Package ratelimit is a keyed token-bucket limiter (per client IP, per
// key) with idle-entry cleanup.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	l    *rate.Limiter
	seen time.Time
}

type Keyed struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	entries map[string]*entry
	last    time.Time
}

// New allows perMinute events per key on average, with the given burst.
func New(perMinute float64, burst int) *Keyed {
	return &Keyed{limit: rate.Limit(perMinute / 60), burst: burst, entries: map[string]*entry{}}
}

func (k *Keyed) Allow(key string) bool {
	now := time.Now()
	k.mu.Lock()
	defer k.mu.Unlock()
	if now.Sub(k.last) > time.Minute {
		for key, e := range k.entries {
			if now.Sub(e.seen) > 10*time.Minute {
				delete(k.entries, key)
			}
		}
		k.last = now
	}
	e, ok := k.entries[key]
	if !ok {
		e = &entry{l: rate.NewLimiter(k.limit, k.burst)}
		k.entries[key] = e
	}
	e.seen = now
	return e.l.AllowN(now, 1)
}
