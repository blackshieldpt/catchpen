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
	"strconv"
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

// Templates is the template categories, their sections and defaults.
func (c *Client) Templates(ctx context.Context) ([]api.TemplateCategory, error) {
	var cs []api.TemplateCategory
	if err := c.get(ctx, api.PathTemplates, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// Tasks is every headless task, newest first.
func (c *Client) Tasks(ctx context.Context) ([]api.TaskInfo, error) {
	var ts []api.TaskInfo
	if err := c.get(ctx, api.PathTasks, &ts); err != nil {
		return nil, err
	}
	return ts, nil
}

// Task is the task with id; one there is none of is an *api.Error with
// api.CodeNotFound.
func (c *Client) Task(ctx context.Context, id string) (*api.TaskInfo, error) {
	var t api.TaskInfo
	if err := c.get(ctx, api.PathTask(id), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// AuditQuery picks a page of the audit log. Zero values: every VM's events,
// api.DefaultAuditLimit of them, the newest.
type AuditQuery struct {
	VM     string
	Limit  int
	Before string // the previous page's Next
}

// Audit is a page of the audit log, newest first.
func (c *Client) Audit(ctx context.Context, q AuditQuery) (*api.AuditPage, error) {
	v := url.Values{}
	if q.VM != "" {
		v.Set("vm", q.VM)
	}
	if q.Limit != 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Before != "" {
		v.Set("before", q.Before)
	}
	path := api.PathAudit
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var p api.AuditPage
	if err := c.get(ctx, path, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// Cache is what the host's package caches hold.
func (c *Client) Cache(ctx context.Context) (*api.CacheInfo, error) {
	var ci api.CacheInfo
	if err := c.get(ctx, api.PathCache, &ci); err != nil {
		return nil, err
	}
	return &ci, nil
}

// CreateVM starts a job creating a VM, as catchpen up does, without
// starting it.
func (c *Client) CreateVM(ctx context.Context, r api.CreateVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMs, r)
}

// StartVM starts a job starting the VM.
func (c *Client) StartVM(ctx context.Context, name string) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMStart(name), nil)
}

// StopVM starts a job shutting the VM down; force powers it off at once.
func (c *Client) StopVM(ctx context.Context, name string, force bool) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMStop(name), api.StopVMRequest{Force: force})
}

// DestroyLoss is what destroying the VM would lose.
func (c *Client) DestroyLoss(ctx context.Context, name string) (*api.VMLoss, error) {
	var l api.VMLoss
	if err := c.get(ctx, api.PathVMDestroy(name), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// DestroyVM starts a job destroying the VM, its disk and snapshots
// included; r confirms it, with the snapshots DestroyLoss told. Without
// Confirm it is an *api.Error with api.CodeConfirmRequired, whose message
// says what would be deleted; with other snapshots than the VM's, with
// api.CodeConflict.
func (c *Client) DestroyVM(ctx context.Context, name string, r api.DestroyVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMDestroy(name), r)
}

// ResumeVM starts a job continuing a VM a canary hit paused.
func (c *Client) ResumeVM(ctx context.Context, name string) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMResume(name), nil)
}

// ResetLoss is what resetting the VM would lose.
func (c *Client) ResetLoss(ctx context.Context, name string) (*api.VMLoss, error) {
	var l api.VMLoss
	if err := c.get(ctx, api.PathVMReset(name), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ResetVM starts a job giving the VM a fresh disk; r confirms it, with the
// snapshots and canary hit ResetLoss told. Without Confirm it is an
// *api.Error with api.CodeConfirmRequired; with others than the VM's, with
// api.CodeConflict.
func (c *Client) ResetVM(ctx context.Context, name string, r api.ResetVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMReset(name), r)
}

// RebaseLoss is what rebasing the VM would lose.
func (c *Client) RebaseLoss(ctx context.Context, name string) (*api.VMLoss, error) {
	var l api.VMLoss
	if err := c.get(ctx, api.PathVMRebase(name), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// RebaseVM starts a job moving the VM's disk onto its template's current
// base; a VM with snapshots needs r to confirm them, as ResetVM.
func (c *Client) RebaseVM(ctx context.Context, name string, r api.RebaseVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMRebase(name), r)
}

// PushCreds starts a job pushing credentials, agent config, env, skills and
// plugins into the running VM.
func (c *Client) PushCreds(ctx context.Context, name string) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMPush(name), nil)
}

// CreateSnapshot starts a job saving a snapshot of the VM.
func (c *Client) CreateSnapshot(ctx context.Context, name string, r api.CreateSnapshotRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathSnapshots(name), r)
}

// RestoreSnapshot starts a job returning the VM to snapshot snap; without
// r.Confirm it is an *api.Error with api.CodeConfirmRequired, and with an
// r.CanaryHit other than the VM's, with api.CodeConflict.
func (c *Client) RestoreSnapshot(ctx context.Context, name, snap string, r api.RestoreSnapshotRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathSnapshotRestore(name, snap), r)
}

// DeleteSnapshot starts a job deleting snapshot snap; without r.Confirm it
// is an *api.Error with api.CodeConfirmRequired.
func (c *Client) DeleteSnapshot(ctx context.Context, name, snap string, r api.DeleteSnapshotRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathSnapshotDelete(name, snap), r)
}

// Egress is the VM's egress mode and, in allowlist mode, its settings and
// lists.
func (c *Client) Egress(ctx context.Context, name string) (*api.EgressInfo, error) {
	var e api.EgressInfo
	if err := c.get(ctx, api.PathEgress(name), &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// EditEgress edits one of the VM's lists, verb being api.EgressAllow,
// api.EgressUnallow, api.EgressDeny or api.EgressUndeny, and returns its
// egress after the change; an entry that isn't a host[:port] is an
// *api.Error with api.CodeBadRequest, nothing changed.
func (c *Client) EditEgress(ctx context.Context, name, verb string, entries []string) (*api.EgressInfo, error) {
	var e api.EgressInfo
	if err := c.do(ctx, http.MethodPost, api.PathEgressEdit(name, verb), api.EgressEntriesRequest{Entries: entries}, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// SetEgressConfig changes the VM's egress settings, and returns its egress
// after the change.
func (c *Client) SetEgressConfig(ctx context.Context, name string, r api.EgressConfigRequest) (*api.EgressInfo, error) {
	var e api.EgressInfo
	if err := c.do(ctx, http.MethodPost, api.PathEgressConfig(name), r, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Forwards lists the VM's live forward sessions.
func (c *Client) Forwards(ctx context.Context, name string) ([]api.ForwardInfo, error) {
	var fs []api.ForwardInfo
	if err := c.get(ctx, api.PathForwards(name), &fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// OpenForward starts a job opening a forward session on the VM.
func (c *Client) OpenForward(ctx context.Context, name string, r api.ForwardRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathForwards(name), r)
}

// StopForwards ends the VM's forward sessions, and returns those left.
func (c *Client) StopForwards(ctx context.Context, name string) ([]api.ForwardInfo, error) {
	var fs []api.ForwardInfo
	if err := c.do(ctx, http.MethodPost, api.PathForwardsStop(name), struct{}{}, &fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// Forges lists the forges VMs clone from.
func (c *Client) Forges(ctx context.Context) ([]api.ForgeInfo, error) {
	var fs []api.ForgeInfo
	if err := c.get(ctx, api.PathForges, &fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// ScanForge is the SSH host keys of a forge to add, to confirm.
func (c *Client) ScanForge(ctx context.Context, r api.ForgeScanRequest) (*api.ForgeScan, error) {
	var s api.ForgeScan
	if err := c.do(ctx, http.MethodPost, api.PathForgeScan, r, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AddForge adds a forge, pinning the host keys r.Fingerprints confirmed;
// keys that changed since are an *api.Error with api.CodeConflict.
func (c *Client) AddForge(ctx context.Context, r api.AddForgeRequest) (*api.ForgeInfo, error) {
	var f api.ForgeInfo
	if err := c.do(ctx, http.MethodPost, api.PathForges, r, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// TestForge checks a forge; a check that failed is the result's Error, not
// an error here.
func (c *Client) TestForge(ctx context.Context, name string) (*api.ForgeCheck, error) {
	var r api.ForgeCheck
	if err := c.do(ctx, http.MethodPost, api.PathForgeTest(name), struct{}{}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteForge removes a forge, and returns those left; without confirm it
// is an *api.Error with api.CodeConfirmRequired.
func (c *Client) DeleteForge(ctx context.Context, name string, confirm bool) ([]api.ForgeInfo, error) {
	var fs []api.ForgeInfo
	if err := c.do(ctx, http.MethodPost, api.PathForgeDelete(name), api.DeleteForgeRequest{Confirm: confirm}, &fs); err != nil {
		return nil, err
	}
	return fs, nil
}

// PullVM starts a job fetching a branch of the VM's into a host repository.
func (c *Client) PullVM(ctx context.Context, name string, r api.PullVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMPull(name), r)
}

// PRVM starts a job opening a pull request from a branch of the VM's.
func (c *Client) PRVM(ctx context.Context, name string, r api.PRVMRequest) (*api.JobInfo, error) {
	return c.job(ctx, api.PathVMPR(name), r)
}

func (c *Client) job(ctx context.Context, path string, in any) (*api.JobInfo, error) {
	var j api.JobInfo
	if err := c.do(ctx, http.MethodPost, path, in, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// Jobs lists the jobs running and the last ones that ended, newest first.
func (c *Client) Jobs(ctx context.Context) ([]api.JobInfo, error) {
	var js []api.JobInfo
	if err := c.get(ctx, api.PathJobs, &js); err != nil {
		return nil, err
	}
	return js, nil
}

// Job is the job with id, with its progress lines from the after-th on.
func (c *Client) Job(ctx context.Context, id string, after int) (*api.Job, error) {
	path := api.PathJob(id)
	if after > 0 {
		path += "?after=" + strconv.Itoa(after)
	}
	var j api.Job
	if err := c.get(ctx, path, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// jobPoll is how often WaitJob asks for a job.
var jobPoll = 500 * time.Millisecond

// WaitJob waits for the job with id to end, and returns it: a job that
// failed has its Error set, and is not an error here. line, if not nil, is
// called with each progress line as it comes; lines a job dropped before
// they were read are not.
func (c *Client) WaitJob(ctx context.Context, id string, line func(string)) (*api.JobInfo, error) {
	after := 0
	for {
		j, err := c.Job(ctx, id, after)
		if err != nil {
			return nil, err
		}
		if line != nil {
			for _, l := range j.Lines {
				line(l)
			}
		}
		after = j.From + len(j.Lines)
		if j.State != api.JobRunning {
			return &j.JobInfo, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(jobPoll):
		}
	}
}

// get decodes path's response into out. A response that isn't a success is
// an *api.Error.
func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// do sends in (nil: no body) as JSON, and decodes the response into out. A
// response that isn't a success (200 or 202) is an *api.Error.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var rb io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rb = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rb)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, maxResponse)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		var er api.ErrorResponse
		if json.NewDecoder(body).Decode(&er) == nil && er.Error != nil {
			return er.Error
		}
		return &api.Error{Code: api.CodeInternal, Message: resp.Status}
	}
	return json.NewDecoder(body).Decode(out)
}
