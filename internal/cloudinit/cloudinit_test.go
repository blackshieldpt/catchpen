package cloudinit

import (
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
	min, err := BuildScript(load(t, "minimal"), "agent")
	if err != nil {
		t.Fatal(err)
	}
	def, err := BuildScript(load(t, "default"), "agent")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(min, "installing docker") || strings.Contains(min, "installing node") {
		t.Error("minimal must not install docker/node")
	}
	for _, want := range []string{"installing docker", "9DC858229FC7DD38854AE2D88D81803C0EBFCD88", "usermod -aG docker agent", "latest-v24.x", "sha256sum -c", "installing uv", "cloud-init clean"} {
		if !strings.Contains(def, want) {
			t.Errorf("default build script missing %q", want)
		}
	}
}

func TestBuildUserData(t *testing.T) {
	m := parse(t, mustB(BuildUserData(load(t, "minimal"), "agent")))
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
	for _, want := range []string{"KbdInteractiveAuthentication no", "PermitRootLogin no", "X11Forwarding no", "PasswordAuthentication no", "AllowUsers agent", "https_proxy=http://10.0.2.100:3128", "Acquire::https::Proxy", "docker.service.d/curral-proxy.conf", `"httpsProxy":"http://10.0.2.100:3128"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("instance user-data missing %q", want)
		}
	}
	in.Egress, in.Docker = "open", false
	raw = string(mustB(InstanceUserData(in)))
	if strings.Contains(raw, "10.0.2.100") {
		t.Error("open egress must not configure the proxy")
	}
}
