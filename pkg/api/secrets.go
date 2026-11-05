package api

import (
	"net/url"
	"strings"
)

// PathSecrets is GET: the names secrets.env sets, as []Secret, sorted;
// never a value. Only names a set takes are listed, and not one whose last
// line has no value (NAME= sets nothing) or a value only of = (a token
// pasted on a line of its own, its padding read as the value).
//
// Every call on secrets.env needs the client's config:write scope
// (CodeForbidden otherwise; a unix socket client has every scope), as the
// configuration files' do. No call returns a value, nor a hash of the file.
// PathAudit shows secret.set and secret.unset's name only to a client with
// config:write.
const PathSecrets = Prefix + "/secrets"

// PathSecret is POST, with a SetSecretRequest: set the secret name in
// secrets.env to Value, and answer with PathSecrets' list. A name secrets.env
// sets already needs Confirm, as its value is lost (CodeConfirmRequired
// otherwise; a value on any line setting name counts). A name is letters,
// digits, _, . and -, not starting with a digit, . or -, up to 128; a value
// is not empty, up to 32 KiB, with no newline, CR or NUL (CodeBadRequest
// otherwise). secrets.env is refused, as a configuration
// file is (CodeConflict), when it may not be replaced: a symbolic link, a
// file with other hard links, another user's, a read-only one or not a
// regular file. Each set is audited as secret.set, with the name.
func PathSecret(name string) string {
	return PathSecrets + "/" + strings.ReplaceAll(url.PathEscape(name), ".", "%2E")
}

// PathSecretDelete is POST, with an UnsetSecretRequest: remove the lines
// setting name from secrets.env (CodeNotFound if none does, a NAME= line
// counting; Confirm needed,
// CodeConfirmRequired otherwise), and answer with PathSecrets' list. Its
// name and file are refused as PathSecret's are, and it is audited as
// secret.unset.
func PathSecretDelete(name string) string { return PathSecret(name) + "/delete" }

// Secret is a name secrets.env sets.
type Secret struct {
	Name string `json:"name"`
	// Shadowed: config.yaml's secrets: has a source of this name, which
	// secret:NAME reads instead of secrets.env. (So does an environment
	// variable of this name in the daemon's environment, which isn't told.)
	Shadowed bool `json:"shadowed"`
}

// SetSecretRequest is a secret's new value.
type SetSecretRequest struct {
	Value   string `json:"value"`
	Confirm bool   `json:"confirm"` // needed to replace a value
}

// UnsetSecretRequest confirms a secret's removal.
type UnsetSecretRequest struct {
	Confirm bool `json:"confirm"`
}
