package egress

import (
	"context"
	"slices"
	"time"
)

// Cursor is where a page ended: the time of its last entry, and how many
// entries at that time earlier pages held.
type Cursor struct {
	Time time.Time
	Skip int
}

type placed struct {
	e   LogEntry
	pos int // its place in the log, the rotated-out file first
}

// newer orders entries newest first: by time, then by their place in the log.
func newer(a, b placed) int {
	if c := b.e.Time.Compare(a.e.Time); c != 0 {
		return c
	}
	return b.pos - a.pos
}

// Page returns up to limit (at least 1) entries, newest first: the first page
// with a nil before, else the one it ends. The cursor it returns ends this
// page; nil means there are no more. It holds about 2*limit entries at a
// time, plus those at before's time.
//
// Pages walk the log as it was: an entry written meanwhile with a time at or
// before the cursor's can be missed (or, at its time, displace one), and a
// rotation drops the oldest entries.
func Page(ctx context.Context, path string, before *Cursor, limit int) ([]LogEntry, *Cursor, error) {
	limit = max(limit, 1)
	skip := 0
	if before != nil {
		skip = max(before.Skip, 0)
	}
	keep := limit + 1 // one more than the page tells whether another follows
	var top, tied []placed
	trim := func() {
		slices.SortFunc(top, newer)
		top = top[:min(len(top), keep)]
	}
	pos := 0
	err := TailEntries(ctx, path, false, func(e LogEntry) error {
		p := placed{e, pos}
		pos++
		switch {
		case before == nil || e.Time.Before(before.Time):
			if top = append(top, p); len(top) >= 2*keep {
				trim()
			}
		case e.Time.Equal(before.Time):
			tied = append(tied, p)
		}
		return nil
	})
	if err == nil {
		err = ctx.Err() // TailEntries stops early, without an error, when ctx is done
	}
	if err != nil {
		return nil, nil, err
	}
	if before != nil {
		// Newest first, those at one time are the log's last first: the first
		// Skip were on earlier pages.
		top = append(top, tied[:max(0, len(tied)-skip)]...)
	}
	trim()
	var next *Cursor
	if len(top) > limit {
		top = top[:limit]
		next = &Cursor{Time: top[limit-1].e.Time}
		if before != nil && next.Time.Equal(before.Time) {
			next.Skip = skip
		}
		for _, p := range top {
			if p.e.Time.Equal(next.Time) {
				next.Skip++
			}
		}
	}
	out := make([]LogEntry, 0, len(top))
	for _, p := range top {
		out = append(out, p.e)
	}
	return out, next, nil
}
