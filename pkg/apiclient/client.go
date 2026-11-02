// Package apiclient is a Go client of the catchpen API (pkg/api), for the
// web cockpit and other programs: over catchpen daemon's unix socket, or over
// TCP with TLS as an enrolled client. It imports only pkg/api and the
// standard library.
package apiclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// maxResponse bounds the response bodies a client reads.
const maxResponse = 16 << 20

// Options says where the API is: Socket, or URL with ServerPin and one of
// Cert and HMAC.
type Options struct {
	// Socket is catchpen daemon's unix socket: api.sock in catchpen's data
	// directory ($CATCHPEN_HOME/data, $XDG_DATA_HOME/catchpen or
	// ~/.local/share/catchpen).
	Socket string

	// URL is a daemon's TCP listener (server.listen): https://host:port.
	URL string
	// ServerPin is the pin of the daemon's certificate, as catchpen daemon
	// info prints it. It is checked instead of a CA.
	ServerPin string
	// Cert is this client's certificate and key, for a client enrolled with
	// catchpen daemon client add --cert.
	Cert *tls.Certificate
	// HMAC is this client's name and key, for a client enrolled with
	// catchpen daemon client add --hmac (the key hex-decoded).
	HMAC *HMAC
}

// HMAC is an HMAC client's name and key.
type HMAC struct {
	Name string
	Key  []byte
}

// Client calls the API. It is safe for concurrent use.
type Client struct {
	hc   *http.Client
	base string
}

// New returns a client of the API that o names.
func New(o Options) (*Client, error) {
	if (o.Socket == "") == (o.URL == "") {
		return nil, errors.New("apiclient: set one of Socket and URL")
	}
	if o.Socket != "" {
		t := &http.Transport{
			Proxy: nil, // a proxy would make the request line absolute
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", o.Socket)
			},
		}
		return &Client{hc: &http.Client{Transport: t}, base: "http://catchpen"}, nil
	}

	u, err := url.Parse(o.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("apiclient: URL must be https://host:port")
	}
	if !api.ValidPin(o.ServerPin) {
		return nil, errors.New("apiclient: ServerPin must be sha256:<64 hex digits>, as catchpen daemon info prints it")
	}
	if (o.Cert == nil) == (o.HMAC == nil) {
		return nil, errors.New("apiclient: set one of Cert and HMAC")
	}
	if o.Cert != nil && len(o.Cert.Certificate) == 0 {
		return nil, errors.New("apiclient: Cert has no certificate")
	}
	pin := o.ServerPin
	tc := &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The pin replaces the CA check: the server's certificate is
		// self-signed. Go still checks that the server holds its key.
		InsecureSkipVerify: true,
		VerifyConnection: func(st tls.ConnectionState) error {
			if len(st.PeerCertificates) == 0 || api.PublicKeyPin(st.PeerCertificates[0]) != pin {
				return errors.New("apiclient: the server's certificate doesn't match ServerPin")
			}
			return nil
		},
	}
	var rt http.RoundTripper
	if o.Cert != nil {
		tc.Certificates = []tls.Certificate{*o.Cert}
		rt = &http.Transport{Proxy: nil, TLSClientConfig: tc, TLSHandshakeTimeout: 10 * time.Second}
	} else {
		if !api.ValidClientName(o.HMAC.Name) || len(o.HMAC.Key) < api.MinKeyBytes {
			return nil, fmt.Errorf("apiclient: HMAC needs a client name and a key of at least %d bytes", api.MinKeyBytes)
		}
		rt = signer{rt: &http.Transport{Proxy: nil, TLSClientConfig: tc, TLSHandshakeTimeout: 10 * time.Second}, name: o.HMAC.Name, key: slices.Clone(o.HMAC.Key)}
	}
	return &Client{hc: &http.Client{Transport: rt}, base: "https://" + u.Host}, nil
}

// signer signs each request it sends (HMAC version 2), with a new nonce.
type signer struct {
	rt   http.RoundTripper
	name string
	key  []byte
}

func (s signer) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		r.Body.Close()
		if err != nil {
			return nil, err
		}
		body = b
	}
	r = r.Clone(r.Context())
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		r.ContentLength = int64(len(body))
	}
	if err := api.Sign(r, s.name, s.key, body); err != nil {
		return nil, err
	}
	return s.rt.RoundTrip(r)
}

// Info is the server's version and API version.
func (c *Client) Info(ctx context.Context) (*api.Info, error) {
	var i api.Info
	if err := c.get(ctx, api.PathInfo, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// VMs lists every VM, warm pool VMs included (Pool).
func (c *Client) VMs(ctx context.Context) ([]api.VMInfo, error) {
	var vms []api.VMInfo
	if err := c.get(ctx, api.PathVMs, &vms); err != nil {
		return nil, err
	}
	return vms, nil
}

// VM is the VM named name; one there is none of is an *api.Error with
// api.CodeNotFound.
func (c *Client) VM(ctx context.Context, name string) (*api.VMInfo, error) {
	var v api.VMInfo
	if err := c.get(ctx, api.PathVM(name), &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Snapshots lists a VM's snapshots, oldest first.
func (c *Client) Snapshots(ctx context.Context, name string) ([]api.SnapshotInfo, error) {
	var ss []api.SnapshotInfo
	if err := c.get(ctx, api.PathSnapshots(name), &ss); err != nil {
		return nil, err
	}
	return ss, nil
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
