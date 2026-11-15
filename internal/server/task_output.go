package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/blackshieldpt/catchpen/internal/sshx"
	"github.com/blackshieldpt/catchpen/pkg/api"
)

// errEnough stops a task-output page that has reached its limit.
var errEnough = errors.New("enough lines")

// taskOutput is GET /v1/tasks/{id}/output: the task's rendered output, one
// NDJSON line at a time, for a config:write client only. A decoy in a line is
// withheld from there on, and acted on.
func (s *Server) taskOutput(w http.ResponseWriter, r *http.Request) {
	if !allowed(w, r, ScopeConfigWrite) { // the output quotes the guest, which holds the template's secrets
		return
	}
	t, m, ok := s.findTask(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	after := 0
	if v := q.Get("after"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, api.CodeBadRequest, "after is a line number (0 or more)")
			return
		}
		after = n
	}
	follow := false
	if v := q.Get("follow"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			writeError(w, api.CodeBadRequest, "follow is true or false")
			return
		}
		follow = b
	}
	limit := 0
	if follow {
		limit = 0 // the whole backlog, then new lines
	} else {
		limit = api.DefaultTaskOutputLines
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > api.MaxTaskOutputLines {
			writeError(w, api.CodeBadRequest, "limit is 0 to "+strconv.Itoa(api.MaxTaskOutputLines))
			return
		}
		limit = n
	}
	m.Client = clientOf(r)     // a canary hit's audit names the reader
	scan := decoyScan(m, t.VM) // lazy: a follow may start before the task's VM exists
	write, end := ndjson(w)
	withheld := ""    // the note, once a decoy (or an unreadable canaries file) is found
	withholdFrom := 0 // the first N the note replaces
	prev := ""
	input := 0          // the N of the line being read
	var pending *string // the line read, held until the next shows whether a decoy straddles
	pendingN := 0
	emitted := 0
	send := func(n int, line string) error {
		if n < after { // paging: the client has this line
			return nil
		}
		if withheld != "" && n >= withholdFrom {
			line = withheld
		}
		if !write(api.TaskOutputLine{N: n, Line: line}) {
			return errClientGone
		}
		emitted++
		if limit > 0 && emitted >= limit {
			return errEnough
		}
		return nil
	}
	flush := func() error {
		if pending == nil {
			return nil
		}
		line, n := *pending, pendingN
		pending = nil
		return send(n, line)
	}
	emit := func(g sshx.GuestOutput) error {
		line := sshx.Printable(string(g.Raw()))
		if withheld == "" {
			switch c, err := scan(prev + line); {
			case err != nil:
				withheld, withholdFrom = withheldUnread, 0
			case c != nil:
				withheld = withheldDecoy
				if c2, _ := scan(line); c2 == nil { // it straddles: the line before holds half
					withholdFrom = input - 1
				} else {
					withholdFrom = input
				}
				if !m.Hit(t.VM) { // once per VM: a page re-read must not act again
					m.CanaryHit(t.VM, c, "in a task's output over the API")
				}
			}
		}
		prev = line
		if err := flush(); err != nil { // the line before, now that this one may complete a decoy
			return err
		}
		l, n := line, input
		input++
		pending, pendingN = &l, n
		return nil
	}
	var err error
	if follow {
		_, err = m.Tasks().FollowOutput(r.Context(), t.ID, false, emit)
	} else {
		err = m.Tasks().TailOutput(r.Context(), t.ID, false, false, emit)
	}
	switch {
	case errors.Is(err, errClientGone), errors.Is(err, errEnough), r.Context().Err() != nil:
		// the client is gone, or it has its page: it resumes with after = last N + 1,
		// and the store re-reads the file from its start and skips
	case err != nil:
		end(&api.Error{Code: api.CodeInternal, Message: "the task's output can't be read"})
	default:
		_ = flush() // the last line; its only errors are the sentinels
	}
}
