package apiclient

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// TaskOutput calls fn with a task's output lines, from after (0-based) on,
// until limit lines (0 means no limit; pass api.DefaultTaskOutputLines for
// the server's default), or with follow until the task ends, or until ctx is
// done. It needs a client enrolled with config:write.
func (c *Client) TaskOutput(ctx context.Context, id string, after, limit int, follow bool, fn func(api.TaskOutputLine) error) error {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.Itoa(after))
	}
	q.Set("limit", strconv.Itoa(limit)) // always: absent would mean the server's 1000
	if follow {
		q.Set("follow", "true")
	}
	path := api.PathTaskOutput(id)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.follow(ctx, path, func(line json.RawMessage) error {
		var l api.TaskOutputLine
		if err := json.Unmarshal(line, &l); err != nil {
			return err
		}
		return fn(l)
	})
}
