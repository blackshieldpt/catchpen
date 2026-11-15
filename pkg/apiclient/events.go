package apiclient

import (
	"context"
	"encoding/json"
	"net/url"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// Events calls fn with each event on the feed, until ctx is done or the
// server closes the stream. after starts the audit part at or after an event
// time (the zero time starts from now); an event at exactly after may repeat,
// so dedupe it. The VM snapshot is always current.
func (c *Client) Events(ctx context.Context, after time.Time, fn func(api.Event) error) error {
	path := api.PathEvents
	if !after.IsZero() {
		path += "?after=" + url.QueryEscape(after.UTC().Format(time.RFC3339Nano))
	}
	return c.follow(ctx, path, func(line json.RawMessage) error {
		var e api.Event
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		return fn(e)
	})
}
