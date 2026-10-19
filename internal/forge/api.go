package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client calls a forge's REST API from the host.
type Client struct {
	F        Forge
	Token    string // API token (Authorization)
	User     string // Gitea basic auth, for minting tokens
	Password string
	Basic    bool // use User/Password for all calls
	HTTP     *http.Client
}

var defaultHTTP = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Host != via[0].URL.Host {
			return errors.New("refusing cross-host or repeated redirect")
		}
		return nil
	},
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

const maxResponse = 4 << 20

func (c *Client) do(ctx context.Context, method, path string, body, out any, basic bool) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.F.APIURL()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case basic || c.Basic:
		req.SetBasicAuth(c.User, c.Password)
	case c.Token != "" && c.F.Type == GitHub:
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case c.Token != "":
		req.Header.Set("Authorization", "token "+c.Token)
	}
	if c.F.Type == GitHub {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.F.Name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("%s %s%s: %w", method, c.F.Name, path, &APIError{Status: resp.StatusCode, Message: printable(msg)})
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s%s: %w", method, c.F.Name, path, err)
		}
	}
	return nil
}

func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
}

func repoAPI(r Repo) string {
	return "/repos/" + url.PathEscape(r.Owner) + "/" + url.PathEscape(r.Name)
}

// User returns the login of the API token's user.
func (c *Client) Login(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	err := c.do(ctx, "GET", "/user", nil, &u, false)
	return u.Login, err
}

// DefaultBranch returns the repository's default branch.
func (c *Client) DefaultBranch(ctx context.Context, r Repo) (string, error) {
	var repo struct {
		DefaultBranch string `json:"default_branch"`
	}
	err := c.do(ctx, "GET", repoAPI(r), nil, &repo, false)
	return repo.DefaultBranch, err
}

// AddDeployKey registers pub on the repo and returns the key id.
func (c *Client) AddDeployKey(ctx context.Context, r Repo, title, pub string, readOnly bool) (int64, error) {
	var k struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, "POST", repoAPI(r)+"/keys", map[string]any{"title": title, "key": pub, "read_only": readOnly}, &k, false)
	return k.ID, err
}

// DeleteDeployKey removes a deploy key; an already-missing key is not an error.
func (c *Client) DeleteDeployKey(ctx context.Context, r Repo, id int64) error {
	err := c.do(ctx, "DELETE", repoAPI(r)+"/keys/"+strconv.FormatInt(id, 10), nil, nil, false)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// MintToken creates a Gitea access token for c.User (basic auth).
func (c *Client) MintToken(ctx context.Context, name string, scopes []string) (int64, string, error) {
	var t struct {
		ID    int64  `json:"id"`
		SHA1  string `json:"sha1"`
		Token string `json:"token"`
	}
	if scopes == nil {
		scopes = []string{}
	}
	err := c.do(ctx, "POST", "/users/"+url.PathEscape(c.User)+"/tokens", map[string]any{"name": name, "scopes": scopes}, &t, true)
	tok := t.SHA1
	if tok == "" {
		tok = t.Token
	}
	if err == nil && tok == "" {
		err = errors.New("gitea returned no token")
	}
	return t.ID, tok, err
}

// DeleteToken deletes a Gitea access token by id; already-missing is not an error.
func (c *Client) DeleteToken(ctx context.Context, id int64) error {
	err := c.do(ctx, "DELETE", "/users/"+url.PathEscape(c.User)+"/tokens/"+strconv.FormatInt(id, 10), nil, nil, true)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// CreatePR opens a pull request and returns its web URL.
func (c *Client) CreatePR(ctx context.Context, r Repo, head, base, title, body string, draft bool) (string, error) {
	req := map[string]any{"title": title, "head": head, "base": base, "body": body}
	if c.F.Type == GitHub {
		req["draft"] = draft
	} else if draft {
		req["title"] = "WIP: " + title
	}
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, "POST", repoAPI(r)+"/pulls", req, &pr, false)
	return pr.HTMLURL, err
}
