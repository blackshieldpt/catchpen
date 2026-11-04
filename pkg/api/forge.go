package api

import (
	"net/url"
	"strings"
)

// PathForges is GET: the forges VMs clone from, as []ForgeInfo in
// forges.yaml's order. It is also POST, with an AddForgeRequest: add a forge,
// as catchpen forge add does, pinning the SSH host keys whose fingerprints
// the client confirmed from PathForgeScan; it answers with the ForgeInfo.
//
// A forge names host secrets that the server sends to the forge's host, so
// adding one, removing one and scanning need the client's config:write
// scope (CodeForbidden otherwise; a unix socket client has every scope).
// Listing and testing don't.
const PathForges = Prefix + "/forges"

// PathForgeScan is POST, with a ForgeScanRequest: the SSH host keys a forge
// to add offers (github.com's from GitHub's API), as ForgeScan.
const PathForgeScan = PathForges + "/scan"

// PathForge is a forge, by name, escaped as PathVM's is.
func PathForge(name string) string {
	return PathForges + "/" + strings.ReplaceAll(url.PathEscape(name), ".", "%2E")
}

// PathForgeTest is POST, with no body (or {}): check the forge's
// credentials, pinned host keys and, in agent mode, host-bound auth, as
// catchpen forge test does; it answers with a ForgeCheck.
func PathForgeTest(name string) string { return PathForge(name) + "/test" }

// PathForgeDelete is POST, with a DeleteForgeRequest: remove the forge, as
// catchpen forge rm does (VMs keep their copy of it). Without Confirm it is
// refused with CodeConfirmRequired. It answers with the forges left,
// []ForgeInfo.
func PathForgeDelete(name string) string { return PathForge(name) + "/delete" }

// PathVMPull is POST, with a PullVMRequest: fetch a branch of one of the
// VM's repos into refs/catchpen/<vm>/<branch> of a host repository, as
// catchpen pull does, as a job.
func PathVMPull(name string) string { return PathVM(name) + "/pull" }

// PathVMPR is POST, with a PRVMRequest: pull a branch, push it with the
// daemon's host git credentials and open a pull request, as catchpen pr
// does, as a job; its last line names the pull request.
func PathVMPR(name string) string { return PathVM(name) + "/pr" }

// ForgeInfo is a forge. Its credentials are named, never given.
type ForgeInfo struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"` // github or gitea
	Host           string   `json:"host"`
	WebURL         string   `json:"web_url"`
	SSHPort        int      `json:"ssh_port"`
	Mode           string   `json:"mode"`                      // deploy-key, minted-token, token, agent or none
	TokenSecret    string   `json:"token_secret,omitempty"`    // the host secret holding its API token
	User           string   `json:"user,omitempty"`            // minted-token: the account tokens are minted for
	PasswordSecret string   `json:"password_secret,omitempty"` // minted-token: the host secret holding that account's password
	Scopes         []string `json:"scopes"`                    // minted tokens' scopes
	HostKeys       []string `json:"host_keys"`                 // the fingerprints of its pinned SSH host keys
}

// ForgeScanRequest is the forge whose SSH host keys to scan.
type ForgeScanRequest struct {
	Type    string `json:"type"`
	Host    string `json:"host"`
	HTTPURL string `json:"http_url,omitempty"` // default https://<host>
	SSHPort int    `json:"ssh_port,omitempty"` // default 22
}

// ForgeScan is a forge's SSH host keys, as fingerprints for a person to
// compare with those the forge publishes.
type ForgeScan struct {
	Fingerprints []string `json:"fingerprints"`
	Published    bool     `json:"published"` // github.com's, from GitHub's API over HTTPS: nothing to compare
}

// AddForgeRequest is a forge to add. Its fingerprints are those a person
// confirmed from a scan: the server scans again and refuses with
// CodeConflict keys that aren't the same. None pins no keys, which only
// the modes that don't use SSH (minted-token, token, none) allow.
type AddForgeRequest struct {
	Name           string   `json:"name"`
	Type           string   `json:"type"`
	Host           string   `json:"host"`
	HTTPURL        string   `json:"http_url,omitempty"`
	SSHPort        int      `json:"ssh_port,omitempty"`
	Mode           string   `json:"mode"`
	TokenSecret    string   `json:"token_secret,omitempty"`    // deploy-key, token
	User           string   `json:"user,omitempty"`            // minted-token
	PasswordSecret string   `json:"password_secret,omitempty"` // minted-token
	Scopes         []string `json:"scopes,omitempty"`          // default write:repository, read:user
	Fingerprints   []string `json:"fingerprints"`
}

// ForgeCheck is what a forge test confirmed, and why it stopped.
type ForgeCheck struct {
	API       bool   `json:"api"`             // its API credentials work
	Login     string `json:"login,omitempty"` // the account they authenticate as
	HostKeys  bool   `json:"host_keys"`       // its SSH server offers only pinned keys
	HostBound bool   `json:"host_bound"`      // its SSH server supports host-bound auth (agent mode)
	Error     string `json:"error,omitempty"` // the check that failed, if one did; can quote the forge
}

// DeleteForgeRequest confirms a forge's removal.
type DeleteForgeRequest struct {
	Confirm bool `json:"confirm"`
}

// PullVMRequest is the branch to pull, and the host repository to fetch it
// into.
type PullVMRequest struct {
	Repo   string `json:"repo,omitempty"`   // a repo name or forge:owner/repo; default: the VM's only one
	Branch string `json:"branch,omitempty"` // default: the clone's current branch
	Into   string `json:"into"`             // the absolute path of a git repository on the daemon's host
}

// PRVMRequest is a pull request to open.
type PRVMRequest struct {
	PullVMRequest
	Title string `json:"title,omitempty"` // default: the last commit's subject
	Body  string `json:"body,omitempty"`  // default: a line naming the VM and branch
	Draft bool   `json:"draft,omitempty"`
	Base  string `json:"base,omitempty"` // default: the repo's default branch
	Head  string `json:"head,omitempty"` // the branch pushed to the forge, under catchpen/; default catchpen/<vm>/<branch>
}
