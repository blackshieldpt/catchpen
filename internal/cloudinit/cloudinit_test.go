package cloudinit

import (
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	tpl "github.com/blackshieldpt/curral/internal/template"
)

func load(t *testing.T, name string) *tpl.Template {
	t.Helper()
	tp, err := tpl.Loader{UserDir: t.TempDir()}.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return tp
}

func parse(t *testing.T, b []byte) map[string]any {
	t.Helper()
	if !strings.HasPrefix(string(b), "#cloud-config\n") {
		t.Fatal("missing #cloud-config header")
	}
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBuildScriptSections(t *testing.T) {
	min, err := BuildScript(load(t, "minimal"), "agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	def, err := BuildScript(load(t, "default"), "agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(min, "installing docker") || strings.Contains(min, "installing node") || strings.Contains(min, "installing agents") {
		t.Error("minimal must not install docker/node/agents")
	}
	agents := "npm install -g --no-fund --no-audit --loglevel=error @anthropic-ai/claude-code@latest opencode-ai@latest @earendil-works/pi-coding-agent@latest"
	if !strings.Contains(def, agents) || strings.Index(def, agents) < strings.Index(def, "installing node") {
		t.Errorf("default build script must install the agents after node")
	}
	for _, want := range []string{"DISABLE_AUTOUPDATER=1\n", "OPENCODE_DISABLE_AUTOUPDATE=1\n", "PI_OFFLINE=1\n", "\nclaude --version\n", "\npi --version\n"} {
		if !strings.Contains(def, want) {
			t.Errorf("default build script missing %q", want)
		}
	}
	for _, want := range []string{"installing docker", "9DC858229FC7DD38854AE2D88D81803C0EBFCD88", "usermod -aG docker agent", "latest-v24.x", "sha256sum -c", "installing uv", "cloud-init clean"} {
		if !strings.Contains(def, want) {
			t.Errorf("default build script missing %q", want)
		}
	}
}

func TestBuildUserData(t *testing.T) {
	m := parse(t, mustB(BuildUserData(load(t, "minimal"), "agent", nil)))
	users := m["users"].([]any)
	u := users[0].(map[string]any)
	if u["name"] != "agent" || u["lock_passwd"] != true {
		t.Errorf("bad user: %v", u)
	}
	run := m["runcmd"].([]any)[0].([]any)
	if !strings.Contains(run[2].(string), BuildOK) || !strings.Contains(run[2].(string), "poweroff") {
		t.Errorf("runcmd lacks marker/poweroff: %v", run)
	}
}

func mustB(b []byte, err error) []byte {
	if err != nil {
		panic(err)
	}
	return b
}

func TestInstanceUserData(t *testing.T) {
	in := Instance{Name: "vm1", User: "agent", AuthorizedKey: "ssh-ed25519 AAAA c\n", HostKey: "PRIV", HostPub: "ssh-ed25519 BBBB h\n", Egress: "allowlist", Docker: true}
	raw := string(mustB(InstanceUserData(in)))
	m := parse(t, []byte(raw))
	if m["ssh_pwauth"] != false || m["hostname"] != "vm1" {
		t.Errorf("unexpected doc: %v", m)
	}
	keys, _ := m["ssh_keys"].(map[string]any)
	if keys["ed25519_private"] != "PRIV" || keys["ed25519_public"] != "ssh-ed25519 BBBB h" {
		t.Errorf("ssh_keys = %v", m["ssh_keys"])
	}
	if g, ok := m["ssh_genkeytypes"].([]any); !ok || len(g) != 0 {
		t.Errorf("ssh_genkeytypes = %v, want [] (host keys come from the seed only)", m["ssh_genkeytypes"])
	}
	users, _ := m["users"].([]any)
	u, _ := users[0].(map[string]any)
	if ak, _ := u["ssh_authorized_keys"].([]any); u["name"] != "agent" || u["lock_passwd"] != true || len(ak) != 1 || ak[0] != "ssh-ed25519 AAAA c" {
		t.Errorf("users = %v", m["users"])
	}
	for _, want := range []string{"KbdInteractiveAuthentication no", "PermitUserEnvironment yes", "PermitRootLogin no", "X11Forwarding no", "PasswordAuthentication no", "AllowUsers agent", "https_proxy=http://10.0.2.100:3128", "Acquire::https::Proxy", "docker.service.d/curral-proxy.conf", `"httpsProxy":"http://10.0.2.100:3128"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("instance user-data missing %q", want)
		}
	}
	if strings.Contains(raw, "10.0.2.102") || strings.Contains(raw, "daemon.json") {
		t.Error("no caches configured without any")
	}
	in.Egress, in.Docker = "open", false
	raw = string(mustB(InstanceUserData(in)))
	if strings.Contains(raw, "10.0.2.100") {
		t.Error("open egress must not configure the proxy")
	}
}

func writeFiles(t *testing.T, m map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range m["write_files"].([]any) {
		f := f.(map[string]any)
		out[f["path"].(string)] += f["content"].(string)
	}
	return out
}

func TestInstanceUserDataCaches(t *testing.T) {
	in := Instance{Name: "vm1", User: "agent", AuthorizedKey: "k", HostKey: "PRIV", HostPub: "p", Egress: "allowlist", Docker: true,
		Cache: []string{"apt", "docker", "go", "npm", "pypi"}}
	m := parse(t, mustB(InstanceUserData(in)))
	files := writeFiles(t, m)
	env := files["/etc/environment"]
	for _, want := range []string{"no_proxy=localhost,127.0.0.1,::1,10.0.2.102\n", "GOPROXY=http://10.0.2.102/go,direct\n",
		"npm_config_registry=http://10.0.2.102/npm/\n", "PIP_INDEX_URL=http://10.0.2.102/pypi/simple/\n", "PIP_TRUSTED_HOST=10.0.2.102\n",
		"UV_DEFAULT_INDEX=http://10.0.2.102/pypi/simple/\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("/etc/environment missing %q:\n%s", want, env)
		}
	}
	if apt := files["/etc/apt/apt.conf.d/94curral-cache"]; !strings.Contains(apt, `Acquire::http::Proxy::deb.debian.org "http://10.0.2.102";`) ||
		!strings.Contains(apt, `Acquire::http::Proxy::archive.ubuntu.com "http://10.0.2.102";`) {
		t.Errorf("apt cache config %q", apt)
	}
	run := m["runcmd"].([]any)
	if mirror := strings.Join(toStrings(run[0].([]any)), " "); !strings.Contains(mirror, `."registry-mirrors" = ["http://10.0.2.102"]`) ||
		strings.Join(toStrings(run[len(run)-1].([]any)), " ") != "systemctl try-restart containerd docker" {
		t.Errorf("runcmd %v", run)
	}
	if !strings.Contains(files["/etc/systemd/system/docker.service.d/curral-proxy.conf"], "NO_PROXY=localhost,127.0.0.1,::1,10.0.2.102") {
		t.Error("docker must reach the mirror directly")
	}

	// Open egress: the caches without the proxy, and docker restarted for its mirror.
	in.Egress, in.Cache = "open", []string{"docker", "npm"}
	m = parse(t, mustB(InstanceUserData(in)))
	files = writeFiles(t, m)
	if env := files["/etc/environment"]; strings.Contains(env, "10.0.2.100") || !strings.Contains(env, "npm_config_registry=") || strings.Contains(env, "GOPROXY") {
		t.Errorf("open /etc/environment %q", env)
	}
	if _, ok := files["/etc/apt/apt.conf.d/94curral-cache"]; ok {
		t.Error("apt cache configured without apt")
	}
	if run := m["runcmd"].([]any); len(run) != 2 || strings.Join(toStrings(run[1].([]any)), " ") != "systemctl try-restart docker" {
		t.Errorf("runcmd %v", run)
	}
}

func toStrings(a []any) []string {
	var out []string
	for _, v := range a {
		out = append(out, v.(string))
	}
	return out
}

func TestBuildScriptCaches(t *testing.T) {
	def := load(t, "default")
	plain, err := BuildScript(def, "agent", nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := BuildScript(def, "agent", []string{"apt", "npm"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"export no_proxy=localhost,127.0.0.1,10.0.2.102 ", "\nexport npm_config_registry=http://10.0.2.102/npm/\n",
		"<<'CURRAL_CACHE'\nAcquire::http::Proxy::archive.ubuntu.com \"http://10.0.2.102\";\n", "\nCURRAL_CACHE\n",
		"\nrm -f /etc/apt/apt.conf.d/94curral-build-cache\n"} {
		if !strings.Contains(s, want) {
			t.Errorf("build script with caches missing %q", want)
		}
	}
	if strings.Contains(plain, "10.0.2.102") || strings.Contains(plain, "CURRAL_CACHE") {
		t.Error("build script without caches mentions them")
	}
	if out, err := exec.Command("bash", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Errorf("bash -n: %v %s", err, out)
	}
}
