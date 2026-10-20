package notify

import (
	"encoding/json"
	"os"
	"time"

	"github.com/blackshieldpt/curral/internal/fsutil"
)

// Limiter is a token bucket kept in a file, shared by the short-lived
// processes that serve one VM's notification requests.
type Limiter struct {
	Path  string
	Burst float64       // bucket size
	Every time.Duration // one token per Every
}

type bucket struct {
	Tokens float64   `json:"tokens"`
	At     time.Time `json:"at"`
}

// Allow takes a token if one is available.
func (l Limiter) Allow(now time.Time) (bool, error) {
	unlock, err := fsutil.Lock(l.Path + ".lock")
	if err != nil {
		return false, err
	}
	defer unlock()
	b := bucket{Tokens: l.Burst, At: now}
	if raw, err := os.ReadFile(l.Path); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	if el := now.Sub(b.At); el > 0 {
		b.Tokens += el.Seconds() / l.Every.Seconds()
	}
	b.Tokens = min(b.Tokens, l.Burst)
	b.At = now
	ok := b.Tokens >= 1
	if ok {
		b.Tokens--
	}
	raw, _ := json.Marshal(b)
	return ok, fsutil.WriteFileAtomic(l.Path, raw, 0o600)
}
