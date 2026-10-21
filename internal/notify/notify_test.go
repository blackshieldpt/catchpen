package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

type secrets map[string]string

func (s secrets) Lookup(_ context.Context, n string) (string, bool, error) {
	v, ok := s[n]
	return v, ok, nil
}

func TestConfigYAML(t *testing.T) {
	var c Config
	err := yaml.Unmarshal([]byte(`
sinks: [desktop, { ntfy: { topic: t1, token: "secret:NTFY" } }, { webhook: { url: "https://h.example/x" } }]
events: [task.done, agent.waiting]
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sinks) != 3 || !c.Sinks[0].Desktop || c.Sinks[1].Ntfy.Topic != "t1" || c.Sinks[2].Webhook.URL != "https://h.example/x" {
		t.Errorf("sinks %+v", c.Sinks)
	}
	if err := c.Validate(); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"sinks: [email]", "sinks: [{ ntfy: {topic: a}, webhook: {url: 'https://x'} }]"} {
		var c Config
		if err := yaml.Unmarshal([]byte(bad), &c); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	for _, c := range []Config{
		{Events: []string{"vm.exploded"}},
		{Sinks: []Sink{{Webhook: &Webhook{URL: "ftp://x"}}}},
		{Sinks: []Sink{{Ntfy: &Ntfy{Topic: "a/b"}}}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestSend(t *testing.T) {
	var mu sync.Mutex
	got := map[string]*http.Request{}
	bodies := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got[r.URL.Path], bodies[r.URL.Path] = r, string(b)
		mu.Unlock()
		if r.URL.Path == "/fail" {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	var desk []string
	n := &Notifier{
		Cfg: Config{Sinks: []Sink{
			{Desktop: true},
			{Ntfy: &Ntfy{URL: srv.URL, Topic: "my topic", Token: "secret:NTFY"}},
			{Webhook: &Webhook{URL: srv.URL + "/hook", Headers: map[string]string{"X-Key": "secret:HOOK"}}},
			{Webhook: &Webhook{URL: srv.URL + "/fail"}},
		}, Events: []string{TaskDone, AgentWaiting}},
		Secrets: secrets{"NTFY": "tok", "HOOK": "k"},
		Desktop: func(_ context.Context, title, msg string) error { desk = append(desk, title+"|"+msg); return nil },
	}
	err := n.Send(context.Background(), Event{Kind: TaskDone, VM: "task-1", Title: "done\x1b]0;x\x07", Message: "line1\nline2 " + strings.Repeat("x", 400)})
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("failing sink not reported: %v", err)
	}
	if len(desk) != 1 || strings.ContainsAny(desk[0], "\x1b\x07\n") {
		t.Errorf("desktop: %q", desk)
	}
	nt := got["/my topic"]
	if nt == nil || nt.Header.Get("Authorization") != "Bearer tok" || nt.Header.Get("Title") == "" || len([]rune(bodies["/my topic"])) > maxMessage+1 {
		t.Errorf("ntfy request: %+v body %q", nt, bodies["/my topic"])
	}
	var ev Event
	if err := json.Unmarshal([]byte(bodies["/hook"]), &ev); err != nil || ev.Kind != TaskDone || ev.VM != "task-1" || got["/hook"].Header.Get("X-Key") != "k" {
		t.Errorf("webhook: %v %+v", err, ev)
	}
	desk = nil
	if err := n.Send(context.Background(), Event{Kind: EgressPrompt}); err != nil || len(desk) != 0 {
		t.Errorf("unsubscribed event sent: %v %v", err, desk)
	}
	var none *Notifier
	if err := none.Send(context.Background(), Event{Kind: TaskDone}); err != nil {
		t.Error(err)
	}
}

func TestLimiter(t *testing.T) {
	l := Limiter{Path: filepath.Join(t.TempDir(), "b"), Burst: 2, Every: time.Minute}
	now := time.Now()
	var got []bool
	for i := 0; i < 3; i++ {
		ok, err := l.Allow(now)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, ok)
	}
	if ok, _ := l.Allow(now.Add(61 * time.Second)); !ok {
		t.Error("no token after refill")
	}
	if got[0] != true || got[1] != true || got[2] != false {
		t.Errorf("burst: %v", got)
	}
}

func TestServeGuest(t *testing.T) {
	var sent []Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e Event
		json.NewDecoder(r.Body).Decode(&e)
		sent = append(sent, e)
	}))
	defer srv.Close()
	n := &Notifier{Cfg: Config{Sinks: []Sink{{Webhook: &Webhook{URL: srv.URL}}}}}
	lim := Limiter{Path: filepath.Join(t.TempDir(), "b"), Burst: 2, Every: time.Hour}
	do := func(req string) string {
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- ServeGuest(context.Background(), b, "vm1", lim, func() (*Notifier, error) { return n, nil })
		}()
		go a.Write([]byte(req))
		resp, err := http.ReadResponse(bufio.NewReader(a), nil)
		a.Close()
		<-done
		if err != nil {
			return "error: " + err.Error()
		}
		return resp.Status
	}
	body := `{"hook_event_name":"Notification","message":"Claude needs your permission\u001b]52;c;x\u0007"}`
	req := "POST /notify?agent=claude HTTP/1.1\r\nHost: x\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if s := do("GET /secrets HTTP/1.1\r\nHost: x\r\n\r\n"); !strings.HasPrefix(s, "404") {
		t.Errorf("other path: %s", s)
	}
	if s := do(req); !strings.HasPrefix(s, "202") {
		t.Fatalf("first: %s", s)
	}
	// The bad request above took a token too: junk can't bypass the limit.
	if s := do(req); !strings.HasPrefix(s, "429") {
		t.Errorf("rate limit: %s", s)
	}
	if len(sent) != 1 || sent[0].Kind != AgentWaiting || sent[0].Title != "curral vm1: claude is waiting" || strings.ContainsAny(sent[0].Message, "\x1b\x07") {
		t.Errorf("sent %+v", sent)
	}
}

func TestDesktopArgsEscapeMarkup(t *testing.T) {
	got := notifySendArgs("t <b>", `<a href="x">click</a> & more`)
	if got[len(got)-1] != "&lt;a href=&#34;x&#34;&gt;click&lt;/a&gt; &amp; more" || got[len(got)-2] != "t <b>" {
		t.Errorf("args %q", got)
	}
}
