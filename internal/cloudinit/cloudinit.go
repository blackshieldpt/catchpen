// Package cloudinit renders NoCloud seeds for template builds and VM instances.
package cloudinit

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"fmt"
	"path"
	"regexp"
	"strings"
	"text/template"

	"go.yaml.in/yaml/v3"

	"github.com/blackshieldpt/curral/internal/egress"
	tpl "github.com/blackshieldpt/curral/internal/template"
)

// Guest-side endpoints served by QEMU guestfwd.
const (
	ProxyAddr = "10.0.2.100"
	ProxyPort = 3128
)

const (
	BuildOK   = "CURRAL-BUILD-OK"
	BuildFail = "CURRAL-BUILD-FAIL"
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

// BuildScript renders the provisioning script for a template.
func BuildScript(t *tpl.Template, user string) (string, error) {
	var scripts []string
	for i, s := range t.Scripts {
		scripts = append(scripts, scriptTarget(i, s))
	}
	pkgs, bins, env := agentInstall(t.Agents)
	var b bytes.Buffer
	err := buildTmpl.Execute(&b, map[string]any{
		"AgentPkgs": pkgs,
		"AgentBins": bins,
		"AgentEnv":  env,
		"User":      user,
		"Packages":  t.Packages,
		"Docker":    t.HasDocker(),
		"Node":      t.Runtimes.Node,
		"Go":        t.Runtimes.Go,
		"UV":        t.Runtimes.Python == "uv",
		"Scripts":   scripts,
		"Proxy":     fmt.Sprintf("http://%s:%d", ProxyAddr, ProxyPort),
	})
	return b.String(), err
}

// agentInstall returns the npm packages, binaries to smoke-test and static
// environment for the template's agents.
func agentInstall(a tpl.Agents) (pkgs, bins, env []string) {
	add := func(ag *tpl.Agent, pkg, bin string, e ...string) {
		if ag == nil {
			return
		}
		v := ag.Version
		if v == "" {
			v = "latest"
		}
		pkgs = append(pkgs, pkg+"@"+v)
		bins = append(bins, bin)
		env = append(env, e...)
	}
	add(a.Claude, "@anthropic-ai/claude-code", "claude", "DISABLE_AUTOUPDATER=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	add(a.OpenCode, "opencode-ai", "opencode", "OPENCODE_DISABLE_AUTOUPDATE=1", "OPENCODE_DISABLE_MODELS_FETCH=1", "OPENCODE_DISABLE_SHARE=1")
	add(a.Pi, "@earendil-works/pi-coding-agent", "pi", "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "PI_TELEMETRY=0")
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
func BuildUserData(t *tpl.Template, user string) ([]byte, error) {
	script, err := BuildScript(t, user)
	if err != nil {
		return nil, err
	}
	files := []writeFile{{
		Path: "/var/lib/curral/build/build.sh", Content: script, Permissions: "0700",
	}}
	for i, s := range t.Scripts {
		files = append(files, writeFile{
			Path:        "/var/lib/curral/build/scripts/" + scriptTarget(i, s),
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
	run := fmt.Sprintf("stty -F /dev/ttyS1 raw -echo; /var/lib/curral/build/build.sh >/dev/ttyS1 2>&1 && echo %s >/dev/ttyS1 || echo %s >/dev/ttyS1; sync; poweroff",
		BuildOK, BuildFail)
	return marshal(map[string]any{
		"users":       []ciUser{{Name: user, Shell: "/bin/bash", Sudo: "ALL=(ALL) NOPASSWD:ALL", LockPasswd: true}},
		"ssh_pwauth":  false,
		"write_files": files,
		"runcmd":      [][]string{{"bash", "-c", run}},
	})
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
}

const sshdConf = `# curral: key-only access
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitRootLogin no
AllowUsers %s
X11Forwarding no
# curral pushes agent credentials to ~/.ssh/environment (0600)
PermitUserEnvironment yes
`

// InstanceUserData is the per-VM seed. It contains the VM's SSH host private key
// (the seed file is 0600 in the VM directory) and no user credentials.
func InstanceUserData(in Instance) ([]byte, error) {
	files := []writeFile{{
		Path: "/etc/ssh/sshd_config.d/00-curral.conf", Content: fmt.Sprintf(sshdConf, in.User), Permissions: "0644",
	}}
	var run [][]string
	if in.Egress == egress.ModeAllowlist {
		proxy := fmt.Sprintf("http://%s:%d", ProxyAddr, ProxyPort)
		noProxy := "localhost,127.0.0.1,::1"
		env := fmt.Sprintf("http_proxy=%[1]s\nhttps_proxy=%[1]s\nHTTP_PROXY=%[1]s\nHTTPS_PROXY=%[1]s\nno_proxy=%[2]s\nNO_PROXY=%[2]s\n", proxy, noProxy)
		files = append(files,
			writeFile{Path: "/etc/environment", Content: env, Append: true},
			writeFile{Path: "/etc/apt/apt.conf.d/95curral-proxy", Permissions: "0644",
				Content: fmt.Sprintf("Acquire::http::Proxy \"%[1]s\";\nAcquire::https::Proxy \"%[1]s\";\n", proxy)},
		)
		if in.Docker {
			unit := fmt.Sprintf("[Service]\nEnvironment=\"HTTP_PROXY=%[1]s\" \"HTTPS_PROXY=%[1]s\" \"NO_PROXY=%[2]s\"\n", proxy, noProxy)
			files = append(files,
				writeFile{Path: "/etc/systemd/system/docker.service.d/curral-proxy.conf", Content: unit, Permissions: "0644"},
				writeFile{Path: "/etc/systemd/system/containerd.service.d/curral-proxy.conf", Content: unit, Permissions: "0644"},
				writeFile{Path: "/home/" + in.User + "/.docker/config.json", Owner: in.User + ":" + in.User, Permissions: "0600", Defer: true,
					Content: fmt.Sprintf(`{"proxies":{"default":{"httpProxy":"%[1]s","httpsProxy":"%[1]s","noProxy":"%[2]s"}}}`+"\n", proxy, noProxy)},
			)
			run = append(run, []string{"systemctl", "daemon-reload"}, []string{"systemctl", "try-restart", "containerd", "docker"})
		}
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
