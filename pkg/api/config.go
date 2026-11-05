package api

// PathConfig is GET: config.yaml, as a ConfigFile. It is also POST, with a
// WriteFileRequest: replace config.yaml with Text, if it is still the file
// BaseHash was read from (CodeConflict otherwise), loads as catchpen loads it
// and breaks no template that resolved, as PathTemplateFile says, nor adds a
// pool entry that doesn't resolve (CodeBadRequest otherwise, nothing
// written); it answers with the ConfigFile written. A write is refused with
// CodeConflict if it changes server: (it would turn off or move the API
// itself) or the file may not be replaced: a symbolic link, a file with
// other hard links, another user's, a read-only one or not a regular file.
//
// The configuration files can hold literal secrets (a template's env and
// MCP headers, notify URLs, headers and tokens), and writing them is host
// code execution (secrets.<name>.command), so every call on them, reads
// too, needs the client's config:write scope (CodeForbidden otherwise; a
// unix socket client has every scope). Each write is audited as
// config.write, with the file and its old and new hashes (PathAudit leaves
// the hashes out for a client without config:write).
const PathConfig = Prefix + "/config"

// PathTemplateFile is GET: the user's category file, templates/<category>.yaml,
// as a TemplateFile. It is also POST, with a WriteFileRequest: write it, as
// for PathConfig; a BaseHash of "" creates it, refused if it exists. A write
// may not break what worked: the category files, if they loaded, must still
// load, and the default template, a profile or a pool entry's template that
// passed a VM create's checks (sections, distro, cloud-init, an allowlist's
// presets and entries; not repos, open egress or ssh.agent restricted, which
// need a flag or a repo) must still pass.
// Its assets (scripts, files) are not written over the API.
func PathTemplateFile(category string) string { return PathTemplates + "/files/" + category }

// PathTemplateFileDelete is POST, with a DeleteFileRequest: remove the user's
// category file, so the built-in one applies, if it is still the file
// BaseHash was read from, may be replaced (as for a write: a regular file of
// the daemon's user with no other link, not read-only), and breaks nothing
// that worked, as for a write. Without Confirm it is refused with
// CodeConfirmRequired. It answers with the TemplateFile left (Exists false).
func PathTemplateFileDelete(category string) string { return PathTemplateFile(category) + "/delete" }

// PathTemplateShow is GET, with the query t=<-t spec> (default: the
// defaults): the template it gives, as catchpen template show prints it, as
// a TemplateShow.
const PathTemplateShow = PathTemplates + "/show"

// ConfigFile is config.yaml.
type ConfigFile struct {
	Text      string `json:"text"`
	Hash      string `json:"hash"` // its SHA-256, hex; "" when there is no file
	Exists    bool   `json:"exists"`
	Effective string `json:"effective"` // the settings it gives, the defaults filled in, as YAML
}

// TemplateFile is one category file: the user's, and the built-in one it
// replaces section by section.
type TemplateFile struct {
	Category string `json:"category"`
	Text     string `json:"text"`
	Hash     string `json:"hash"` // its SHA-256, hex; "" when there is no file
	Exists   bool   `json:"exists"`
	Builtin  string `json:"builtin"` // the built-in file's text ("" for none)
}

// WriteFileRequest is a configuration file's new text, and the hash of the
// file it was made from.
type WriteFileRequest struct {
	Text     string `json:"text"`
	BaseHash string `json:"base_hash"` // "" for a file that doesn't exist
}

// DeleteFileRequest confirms a file's removal.
type DeleteFileRequest struct {
	BaseHash string `json:"base_hash"`
	Confirm  bool   `json:"confirm"`
}

// TemplateShow is the template a -t spec gives.
type TemplateShow struct {
	Spec  string         `json:"spec"`  // normalized
	Parts []TemplatePart `json:"parts"` // the sections each category gave, in merge order
	Base  string         `json:"base"`  // the base image's path on the daemon's host
	YAML  string         `json:"yaml"`  // its settings
}

// TemplatePart is the sections one category gave a template.
type TemplatePart struct {
	Category string   `json:"category"`
	Sections []string `json:"sections"`
	Default  bool     `json:"default"` // the category's default, unchanged
}
