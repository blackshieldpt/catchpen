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

var defaultHTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: checkRedirect}

// checkRedirect is the redirect policy of every forge API call: the same host,
// never from https to http, and never a change of method (Go turns a POST or
// DELETE into a GET on a 301, 302 or 303).
func checkRedirect(req *http.Request, via []*http.Request) error {
	first := via[0]
	switch {
	case len(via) >= 5 || req.URL.Host != first.URL.Host:
		return errors.New("refusing cross-host or repeated redirect")
	case first.URL.Scheme == "https" && req.URL.Scheme != "https":
		return errors.New("refusing a redirect from https to http")
	case req.Method != first.Method:
		return fmt.Errorf("refusing a redirect that turns %s into %s (to %s; check the repo name or http_url)", first.Method, req.Method, req.URL.EscapedPath())
	}
	return nil
}

// errBody marks a 2xx answer whose body couldn't be read or decoded: the forge
// may have acted without saying how.
var errBody = errors.New("unreadable answer")

// ErrMayRemain marks a failed add or mint whose credential the forge may have
// created and catchpen couldn't remove.
var ErrMayRemain = errors.New("may remain")

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
	} else if hc.CheckRedirect == nil { // a caller's client gets the same policy
		cp := *hc
		cp.CheckRedirect = checkRedirect
		hc = &cp
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, c.F.Name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("%s %s%s: %w: %v", method, c.F.Name, path, errBody, err)
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
			return fmt.Errorf("%s %s%s: %w: %v", method, c.F.Name, path, errBody, err)
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
	if errors.Is(err, errBody) || err == nil && k.ID == 0 {
		return 0, c.dropDeployKey(ctx, r, title, pub, err)
	}
	return k.ID, err
}

// dropDeployKey removes key pub, which an add answered without its id, so the
// repo isn't left with a key nothing tracks. It returns the add's error.
func (c *Client) dropDeployKey(ctx context.Context, r Repo, title, pub string, cause error) error {
	if cause == nil {
		cause = errors.New("the forge returned no key id")
	}
	var keys []struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	err := c.do(ctx, "GET", repoAPI(r)+"/keys?per_page=100&limit=50", nil, &keys, false)
	if err == nil {
		err = errors.New("not in the first page of the repo's keys")
		for _, k := range keys {
			if sameKey(k.Key, pub) {
				if err = c.DeleteDeployKey(ctx, r, k.ID); err == nil {
					return fmt.Errorf("%w (the forge added the key; it was removed)", cause)
				}
				break
			}
		}
	}
	return fmt.Errorf("%w; deploy key %q %w (remove it in the repo's settings): %v", cause, title, ErrMayRemain, err)
}

// sameKey compares two authorized_keys lines by type and key, not comment.
func sameKey(a, b string) bool {
	fa, fb := strings.Fields(a), strings.Fields(b)
	return len(fa) >= 2 && len(fb) >= 2 && fa[0] == fb[0] && fa[1] == fb[1]
}

// DeleteDeployKey removes a deploy key; an already-missing key is not an error.
// A forge also answers 404 when the token can no longer manage the repo's keys
// (it lost access or admin rights), so a 404 counts as removed only while the
// token is still the repo's admin.
func (c *Client) DeleteDeployKey(ctx context.Context, r Repo, id int64) error {
	err := c.do(ctx, "DELETE", repoAPI(r)+"/keys/"+strconv.FormatInt(id, 10), nil, nil, false)
	if !IsNotFound(err) {
		return err
	}
	var repo struct {
		Permissions struct {
			Admin bool `json:"admin"`
		} `json:"permissions"`
	}
	if rerr := c.do(ctx, "GET", repoAPI(r), nil, &repo, false); rerr != nil {
		return fmt.Errorf("deploy key %d on %s: not found, and the repo isn't visible to the token (%v); the key may remain", id, r, rerr)
	}
	if !repo.Permissions.Admin {
		return fmt.Errorf("deploy key %d on %s: not found, and the token isn't the repo's admin; the key may remain", id, r)
	}
	return nil
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
	if errors.Is(err, errBody) || err == nil && (tok == "" || t.ID == 0) {
		return 0, "", c.dropToken(ctx, name, err)
	}
	return t.ID, tok, err
}

// dropToken deletes token name, which a mint answered without its secret or
// id, so the account isn't left with a token nothing tracks. It returns the
// mint's error.
func (c *Client) dropToken(ctx context.Context, name string, cause error) error {
	if cause == nil {
		cause = errors.New("gitea returned no token or no id")
	}
	var toks []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	}
	err := c.do(ctx, "GET", "/users/"+url.PathEscape(c.User)+"/tokens?limit=50", nil, &toks, true)
	if err == nil {
		err = errors.New("not in the first page of the account's tokens")
		for _, t := range toks {
			if t.Name == name {
				if err = c.DeleteToken(ctx, t.ID); err == nil {
					return fmt.Errorf("%w (the token was minted; it was deleted)", cause)
				}
				break
			}
		}
	}
	return fmt.Errorf("%w; token %q %w (delete it in %s's settings): %v", cause, name, ErrMayRemain, c.User, err)
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
