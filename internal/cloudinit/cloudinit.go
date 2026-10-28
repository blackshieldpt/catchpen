// Package cloudinit renders NoCloud seeds for template builds and VM instances.
package cloudinit

import (
	"bytes"
	"cmp"
	_ "embed"
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/catchpen/internal/cache"
	"github.com/blackshieldpt/catchpen/internal/canary"
	"github.com/blackshieldpt/catchpen/internal/egress"
	tpl "github.com/blackshieldpt/catchpen/internal/template"
)

// Guest-side endpoints served by QEMU guestfwd.
const (
	ProxyAddr = "10.0.2.100"
	ProxyPort = 3128
	// NotifyAddr:NotifyPort reaches catchpen notify-stdio on the host (agent hooks).
	NotifyAddr = "10.0.2.101"
	NotifyPort = 80
)

// Claude Code's token or key goes to ClaudeEnvFile (under the guest home),
// which only the base's claude wrapper loads before running ClaudeReal.
const (
	ClaudeReal    = "/usr/local/lib/catchpen/claude"
	ClaudeEnvFile = ".config/catchpen/claude.env"
	withEnv       = "/usr/local/lib/catchpen/with-env"
)

const (
	BuildOK   = "CATCHPEN-BUILD-OK"
	BuildFail = "CATCHPEN-BUILD-FAIL"
)

//go:embed build.sh.tmpl
var buildScript string

var buildTmpl = template.Must(template.New("build").Funcs(template.FuncMap{"join": strings.Join}).Parse(buildScript))

type writeFile struct {
	Path        string `yaml:"path"`
	Content     string `yaml:"content,omitempty"`
	Encoding    string `yaml:"encoding,omitempty"`
	Owner       string `yaml:"owner,omitempty"`
	Permissions string `yaml:"permissions,omitempty"`
	Append      bool   `yaml:"append,omitempty"`
	Defer       bool   `yaml:"defer,omitempty"`
}

type ciUser struct {
	Name              string   `yaml:"name"`
	Shell             string   `yaml:"shell"`
	Sudo              string   `yaml:"sudo"`
	LockPasswd        bool     `yaml:"lock_passwd"`
	SSHAuthorizedKeys []string `yaml:"ssh_authorized_keys,omitempty"`
}

var scriptNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// scriptTarget maps a template script reference to a unique file name in the guest.
func scriptTarget(i int, ref string) string {
	return fmt.Sprintf("%02d-%s", i, scriptNameRe.ReplaceAllString(path.Base(ref), "_"))
}

func homePath(dst, user string) string {
	if rest, ok := strings.CutPrefix(dst, "~/"); ok {
		return "/home/" + user + "/" + rest
	}
	return dst
}

// BuildScript renders the provisioning script for a template; caches are
// used during the build only.
func BuildScript(t *tpl.Template, user string, caches []string) (string, error) {
	var scripts []string
	for i, s := range t.Scripts {
		scripts = append(scripts, scriptTarget(i, s))
	}
	pkgs, bins, env := agentInstall(t.Agents)
	var b bytes.Buffer
	err := buildTmpl.Execute(&b, map[string]any{
		"GH":            slices.Contains(t.Tools, "gh"),
		"Tea":           slices.Contains(t.Tools, "tea"),
		"AgentPkgs":     pkgs,
		"AgentBins":     bins,
		"AgentEnv":      env,
		"Claude":        t.Agents.Claude != nil,
		"ClaudeReal":    ClaudeReal,
		"ClaudeEnvFile": ClaudeEnvFile,
		"WithEnv":       withEnv,
		"User":          user,
		"Packages":      slices.Sorted(slices.Values(t.Packages)), // in any order: the same packages share a base
		"Docker":        t.HasDocker(),
		"Node":          t.Runtimes.Node,
		"Go":            t.Runtimes.Go,
		"UV":            t.Runtimes.Python != "",
		"Python":        strings.TrimPrefix(t.Runtimes.Python, "uv"),
		"Scripts":       scripts,
		"OMP":           ompTag(t.Agents.OMP),
		"Proxy":         fmt.Sprintf("http://%s:%d", ProxyAddr, ProxyPort),
		"Caches":        len(caches) > 0,
		"CacheAddr":     cache.Addr,
		"CacheEnv":      cacheEnv(caches),
		"CacheApt":      cacheApt(caches),
	})
	return b.String(), err
}

// ompTag is the oh-my-pi release a template installs ("" for none): omp is
// a single binary, not an npm package.
func ompTag(a *tpl.Agent) string {
	switch {
	case a == nil:
		return ""
	case a.Version == "" || a.Version == "latest":
		return "latest"
	}
	return "v" + a.Version
}

// agentInstall returns the npm packages, binaries to smoke-test and static
// environment for the template's agents.
func agentInstall(a tpl.Agents) (pkgs, bins, env []string) {
	for _, d := range tpl.AgentDefs {
		ag := *d.Get(&a)
		if ag == nil || d.Package == "" {
			continue
		}
		pkgs = append(pkgs, d.Package+"@"+cmp.Or(ag.Version, "latest"))
		bins = append(bins, d.Name)
		env = append(env, d.Env...)
	}
	return pkgs, bins, env
}

func marshal(doc map[string]any) ([]byte, error) {
	b, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return append([]byte("#cloud-config\n"), b...), nil
}

// BuildUserData is the seed for building a template base. The second serial port
// receives the build log and a final OK/FAIL marker; the VM powers off after.
func BuildUserData(t *tpl.Template, user string, caches []string) ([]byte, error) {
	script, err := BuildScript(t, user, caches)
	if err != nil {
		return nil, err
	}
	files := []writeFile{{
		Path: "/var/lib/catchpen/build/build.sh", Content: script, Permissions: "0700",
	}}
	for i, s := range t.Scripts {
		files = append(files, writeFile{
			Path:        "/var/lib/catchpen/build/scripts/" + scriptTarget(i, s),
			Content:     base64.StdEncoding.EncodeToString(t.Asset(s)),
			Encoding:    "b64",
			Permissions: "0700",
		})
	}
	for _, f := range t.Files {
		mode := f.Mode
		if mode == "" {
			mode = "0644"
		}
		wf := writeFile{
			Path:        homePath(f.Dst, user),
			Content:     base64.StdEncoding.EncodeToString(t.Asset(f.Src)),
			Encoding:    "b64",
			Permissions: mode,
		}
		if strings.HasPrefix(f.Dst, "~/") {
			wf.Owner, wf.Defer = user+":"+user, true
		}
		files = append(files, wf)
	}
	// ttyS1 is a dedicated build-log port (ttyS0 carries the console and a getty).
	run := fmt.Sprintf("stty -F /dev/ttyS1 raw -echo; /var/lib/catchpen/build/build.sh >/dev/ttyS1 2>&1 && echo %s >/dev/ttyS1 || echo %s >/dev/ttyS1; sync; poweroff",
		BuildOK, BuildFail)
	return marshal(map[string]any{
		"users":       []ciUser{{Name: user, Shell: "/bin/bash", Sudo: "ALL=(ALL) NOPASSWD:ALL", LockPasswd: true}},
		"ssh_pwauth":  false,
		"write_files": files,
		"runcmd":      [][]string{{"bash", "-c", run}},
	})
}

// cacheEnv points package managers at the caches.
func cacheEnv(kinds []string) []string {
	var env []string
	for _, k := range kinds {
		switch k {
		case "go":
			env = append(env, "GOPROXY="+cache.Base+"/go,direct")
		case "npm":
			env = append(env, "npm_config_registry="+cache.Base+"/npm/", "YARN_NPM_REGISTRY_SERVER="+cache.Base+"/npm",
				"YARN_UNSAFE_HTTP_WHITELIST="+cache.Addr)
		case "pypi":
			env = append(env, "PIP_INDEX_URL="+cache.Base+"/pypi/simple/", "PIP_TRUSTED_HOST="+cache.Addr,
				"UV_DEFAULT_INDEX="+cache.Base+"/pypi/simple/")
		}
	}
	return env
}

// cacheApt is the apt configuration fetching the cached mirrors through the cache.
func cacheApt(kinds []string) string {
	if !slices.Contains(kinds, "apt") {
		return ""
	}
	var b strings.Builder
	for _, h := range cache.AptHosts {
		fmt.Fprintf(&b, "Acquire::http::Proxy::%s \"%s\";\n", h, cache.Base)
	}
	return b.String()
}

// Instance holds per-VM first-boot settings.
type Instance struct {
	Name          string
	User          string
	AuthorizedKey string // client public key, authorized_keys format
	HostKey       string // ed25519 private key, OpenSSH PEM
	HostPub       string // ed25519 public key, authorized_keys format
	Egress        string // allowlist | open | none
	Docker        bool
	Cache         []string           // cache kinds the VM uses
	Canaries      []canary.GuestFile // decoys for the user's home
}

const sshdConf = `# catchpen: key-only access
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers %s
X11Forwarding no
# catchpen pushes the template's env: values to ~/.ssh/environment (0600)
PermitUserEnvironment yes
`

// InstanceUserData is the per-VM seed. It contains the VM's SSH host private key
// (the seed file is 0600 in the VM directory) and no user credentials.
func InstanceUserData(in Instance) ([]byte, error) {
	files := []writeFile{{
		Path: "/etc/ssh/sshd_config.d/00-catchpen.conf", Content: fmt.Sprintf(sshdConf, in.User), Permissions: "0644",
	}}
	var run [][]string
	noProxy := "localhost,127.0.0.1,::1"
	if len(in.Cache) > 0 {
		noProxy += "," + cache.Addr
	}
	if in.Egress == egress.ModeAllowlist {
		proxy := fmt.Sprintf("http://%s:%d", ProxyAddr, ProxyPort)
		env := fmt.Sprintf("http_proxy=%[1]s\nhttps_proxy=%[1]s\nHTTP_PROXY=%[1]s\nHTTPS_PROXY=%[1]s\nno_proxy=%[2]s\nNO_PROXY=%[2]s\n", proxy, noProxy)
		files = append(files,
			writeFile{Path: "/etc/environment", Content: env, Append: true},
			writeFile{Path: "/etc/apt/apt.conf.d/95catchpen-proxy", Permissions: "0644",
				Content: fmt.Sprintf("Acquire::http::Proxy \"%[1]s\";\nAcquire::https::Proxy \"%[1]s\";\n", proxy)},
		)
		if in.Docker {
			unit := fmt.Sprintf("[Service]\nEnvironment=\"HTTP_PROXY=%[1]s\" \"HTTPS_PROXY=%[1]s\" \"NO_PROXY=%[2]s\"\n", proxy, noProxy)
			files = append(files,
				writeFile{Path: "/etc/systemd/system/docker.service.d/catchpen-proxy.conf", Content: unit, Permissions: "0644"},
				writeFile{Path: "/etc/systemd/system/containerd.service.d/catchpen-proxy.conf", Content: unit, Permissions: "0644"},
				writeFile{Path: "/home/" + in.User + "/.docker/config.json", Owner: in.User + ":" + in.User, Permissions: "0600", Defer: true,
					Content: fmt.Sprintf(`{"proxies":{"default":{"httpProxy":"%[1]s","httpsProxy":"%[1]s","noProxy":"%[2]s"}}}`+"\n", proxy, noProxy)},
			)
			run = append(run, []string{"systemctl", "daemon-reload"}, []string{"systemctl", "try-restart", "containerd", "docker"})
		}
	}
	if env := cacheEnv(in.Cache); len(env) > 0 {
		files = append(files, writeFile{Path: "/etc/environment", Content: strings.Join(env, "\n") + "\n", Append: true})
	}
	if apt := cacheApt(in.Cache); apt != "" {
		files = append(files, writeFile{Path: "/etc/apt/apt.conf.d/94catchpen-cache", Content: apt, Permissions: "0644"})
	}
	if in.Docker && slices.Contains(in.Cache, "docker") {
		// Merged into what the template's base may have in daemon.json.
		mirror := fmt.Sprintf(`f=/etc/docker/daemon.json; [ -s $f ] || echo {} >$f; `+
			`jq '."registry-mirrors" = ["%[1]s"] + ((."registry-mirrors" // []) - ["%[1]s"])' $f >$f.new && mv $f.new $f`, cache.Base)
		run = append([][]string{{"sh", "-c", mirror}}, run...)
		if len(run) == 1 {
			run = append(run, []string{"systemctl", "try-restart", "docker"})
		}
	}
	home := "/home/" + in.User + "/"
	var dirs []string
	for _, f := range in.Canaries {
		files = append(files, writeFile{Path: home + f.Path, Content: f.Content, Owner: in.User + ":" + in.User, Permissions: "0600", Defer: true})
		if d := path.Dir(f.Path); d != "." && !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs { // cloud-init makes parent directories root's
		run = append(run, []string{"chown", in.User + ":" + in.User, home + d})
	}
	doc := map[string]any{
		"hostname":          in.Name,
		"preserve_hostname": false,
		"manage_etc_hosts":  true,
		"ssh_pwauth":        false,
		"disable_root":      true,
		"users": []ciUser{{
			Name: in.User, Shell: "/bin/bash", Sudo: "ALL=(ALL) NOPASSWD:ALL", LockPasswd: true,
			SSHAuthorizedKeys: []string{strings.TrimSpace(in.AuthorizedKey)},
		}},
		"ssh_deletekeys":  true,
		"ssh_genkeytypes": []string{},
		"ssh_keys": map[string]string{
			"ed25519_private": in.HostKey,
			"ed25519_public":  strings.TrimSpace(in.HostPub),
		},
		"write_files": files,
	}
	if len(run) > 0 {
		doc["runcmd"] = run
	}
	return marshal(doc)
}

func MetaData(instanceID, hostname string) []byte {
	return []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", instanceID, hostname))
}
