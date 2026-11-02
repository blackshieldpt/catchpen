// Package apiclient is a Go client of the catchpen API (pkg/api), for the
// web cockpit and other programs. It imports only pkg/api and the standard
// library.
package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// maxResponse bounds the response bodies a client reads.
const maxResponse = 16 << 20

// Options says where the API is.
type Options struct {
	// Socket is catchpen daemon's unix socket: api.sock in catchpen's data
	// directory ($CATCHPEN_HOME/data, $XDG_DATA_HOME/catchpen or
	// ~/.local/share/catchpen).
	Socket string
}

// Client calls the API. It is safe for concurrent use.
type Client struct {
	hc   *http.Client
	base string
}

// New returns a client of the API that o names.
func New(o Options) (*Client, error) {
	if o.Socket == "" {
		return nil, errors.New("apiclient: no socket")
	}
	t := &http.Transport{
		Proxy: nil, // a proxy would make the request line absolute
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", o.Socket)
		},
	}
	return &Client{hc: &http.Client{Transport: t}, base: "http://catchpen"}, nil
}

// Info is the server's version and API version.
func (c *Client) Info(ctx context.Context) (*api.Info, error) {
	var i api.Info
	if err := c.get(ctx, api.PathInfo, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// get decodes path's response into out. A response that isn't a success is
// an *api.Error.
func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxResponse)
	if resp.StatusCode != http.StatusOK {
		var er api.ErrorResponse
		if json.NewDecoder(body).Decode(&er) == nil && er.Error != nil {
			return er.Error
		}
		return &api.Error{Code: api.CodeInternal, Message: resp.Status}
	}
	return json.NewDecoder(body).Decode(out)
}
