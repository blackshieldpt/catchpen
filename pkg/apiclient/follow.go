package apiclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// follow reads a follow response: one JSON value per line. An error line is
// returned as an *api.Error; fn's error stops it.
func (c *Client) follow(ctx context.Context, path string, fn func(json.RawMessage) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var er api.ErrorResponse
		if json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&er) == nil && er.Error != nil {
			return er.Error
		}
		return &api.Error{Code: api.CodeInternal, Message: resp.Status}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20) // an audit detail can be large
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var er api.ErrorResponse
		if json.Unmarshal(line, &er) == nil && er.Error != nil {
			return er.Error
		}
		if err := fn(line); err != nil {
			return err
		}
	}
	return sc.Err()
}

// AuditFollow calls fn with each new audit event, oldest first, until ctx is
// done or the server closes the stream. after is an event time: events before
// it are skipped (the zero time starts from now); one at exactly after may
// repeat, so dedupe it.
func (c *Client) AuditFollow(ctx context.Context, vm string, after time.Time, fn func(api.AuditEvent) error) error {
	q := url.Values{}
	if vm != "" {
		q.Set("vm", vm)
	}
	if !after.IsZero() {
		q.Set("after", after.UTC().Format(time.RFC3339Nano))
	}
	path := api.PathAuditFollow
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	return c.follow(ctx, path, func(line json.RawMessage) error {
		var e api.AuditEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		return fn(e)
	})
}

// EgressLog is one page of a VM's egress log, newest first. before is a
// previous page's Next ("" for the first page); limit 0 means the default.
func (c *Client) EgressLog(ctx context.Context, vm string, limit int, before string) (api.EgressLogPage, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before != "" {
		q.Set("before", before)
	}
	path := api.PathEgressLog(vm)
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var page api.EgressLogPage
	err := c.get(ctx, path, &page)
	return page, err
}

// EgressLogFollow calls fn with each new egress log entry, oldest first,
// until ctx is done or the server closes the stream. after works as in
// AuditFollow.
func (c *Client) EgressLogFollow(ctx context.Context, vm string, after time.Time, fn func(api.EgressLogEntry) error) error {
	path := api.PathEgressLogFollow(vm)
	if !after.IsZero() {
		path += "?after=" + url.QueryEscape(after.UTC().Format(time.RFC3339Nano))
	}
	return c.follow(ctx, path, func(line json.RawMessage) error {
		var e api.EgressLogEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		return fn(e)
	})
}
