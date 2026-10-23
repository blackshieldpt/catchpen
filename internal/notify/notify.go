// Package notify sends curral events (task results, pending egress prompts,
// agents waiting for input) to the user's notification sinks. Sink
// credentials stay on the host; guest-originated text is sanitized and
// rate-limited before it gets here.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os/exec"
	"reflect"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Event kinds.
const (
	TaskDone     = "task.done"
	TaskFailed   = "task.failed"
	EgressPrompt = "egress.prompt"
	AgentWaiting = "agent.waiting"
	VMIdleStop   = "vm.idle_stop"
	CanaryHit    = "canary.hit"
)

// Kinds lists the event kinds a config may name.
var Kinds = []string{TaskDone, TaskFailed, EgressPrompt, AgentWaiting, VMIdleStop, CanaryHit}

// Config is the notify: section of config.yaml.
type Config struct {
	Sinks  []Sink   `yaml:"sinks"`
	Events []string `yaml:"events"` // default: all kinds
}

// Sink is one destination: "desktop", {ntfy: ...} or {webhook: ...}.
type Sink struct {
	Desktop bool
	Ntfy    *Ntfy    `yaml:"ntfy"`
	Webhook *Webhook `yaml:"webhook"`
}

// Ntfy posts to an ntfy server (https://ntfy.sh or your own).
type Ntfy struct {
	URL   string `yaml:"url"`
	Topic string `yaml:"topic"`
	Token string `yaml:"token"` // secret:NAME
}

// Webhook POSTs the event as JSON.
type Webhook struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers"` // values may be secret:NAME
}

// knownKeys refuses keys of mapping n that struct t has no field for, as
// config.yaml's strict decoding would: Node.Decode doesn't check them.
func knownKeys(n *yaml.Node, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Kind != yaml.MappingNode || t.Kind() != reflect.Struct {
		return nil
	}
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ","); name != "" && name != "-" {
			fields[name] = t.Field(i).Type
		}
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		k := n.Content[i]
		ft, ok := fields[k.Value]
		if !ok {
			return fmt.Errorf("line %d: field %s not found in type %s", k.Line, k.Value, t)
		}
		if err := knownKeys(n.Content[i+1], ft); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sink) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		if n.Value != "desktop" {
			return fmt.Errorf("line %d: unknown sink %q (desktop, ntfy or webhook)", n.Line, n.Value)
		}
		s.Desktop = true
		return nil
	}
	var m struct {
		Ntfy    *Ntfy    `yaml:"ntfy"`
		Webhook *Webhook `yaml:"webhook"`
	}
	if err := knownKeys(n, reflect.TypeOf(m)); err != nil {
		return err
	}
	if err := n.Decode(&m); err != nil {
		return err
	}
	if (m.Ntfy == nil) == (m.Webhook == nil) {
		return fmt.Errorf("line %d: a sink is desktop, {ntfy: ...} or {webhook: ...}", n.Line)
	}
	s.Ntfy, s.Webhook = m.Ntfy, m.Webhook
	return nil
}

// Validate checks the config.
func (c Config) Validate() error {
	for _, e := range c.Events {
		if !contains(Kinds, e) {
			return fmt.Errorf("notify.events: unknown event %q (known: %s)", e, strings.Join(Kinds, ", "))
		}
	}
	for _, s := range c.Sinks {
		var raw string
		switch {
		case s.Ntfy != nil:
			if s.Ntfy.Topic == "" || strings.ContainsAny(s.Ntfy.Topic, "/?#") {
				return fmt.Errorf("notify: ntfy needs a topic (no / ? #)")
			}
			raw = s.Ntfy.URL
			if raw == "" {
				raw = "https://ntfy.sh"
			}
		case s.Webhook != nil:
			raw = s.Webhook.URL
		default:
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("notify: invalid sink URL %q", raw)
		}
	}
	return nil
}

// Event is one notification.
type Event struct {
	Kind    string    `json:"event"`
	VM      string    `json:"vm,omitempty"`
	Title   string    `json:"title"`
	Message string    `json:"message"`
	Time    time.Time `json:"time"`
}

// Secrets resolves secret:NAME values.
type Secrets interface {
	Lookup(ctx context.Context, name string) (string, bool, error)
}

// Notifier delivers events to the configured sinks.
type Notifier struct {
	Cfg     Config
	Secrets Secrets
	HTTP    *http.Client
	// Desktop runs the desktop notifier (default: notify-send).
	Desktop func(ctx context.Context, title, message string) error
}

// Wants reports whether events of this kind are delivered at all.
func (n *Notifier) Wants(kind string) bool {
	return n != nil && len(n.Cfg.Sinks) > 0 && (len(n.Cfg.Events) == 0 || contains(n.Cfg.Events, kind))
}

const (
	maxTitle   = 80
	maxMessage = 300
)

// Clean makes text safe to show: printable characters only, newlines become
// spaces, and it is cut to max runes.
func Clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			b.WriteString("…")
			break
		}
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			r = ' '
		case !unicode.IsPrint(r):
			continue
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}

// Send delivers e to every sink. Failures are joined; one sink failing
// doesn't stop the others.
func (n *Notifier) Send(ctx context.Context, e Event) error {
	if !n.Wants(e.Kind) {
		return nil
	}
	e.Title, e.Message = Clean(e.Title, maxTitle), Clean(e.Message, maxMessage)
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var errs []error
	for _, s := range n.Cfg.Sinks {
		var err error
		switch {
		case s.Desktop:
			err = n.desktop(ctx, e)
		case s.Ntfy != nil:
			err = n.ntfy(ctx, *s.Ntfy, e)
		case s.Webhook != nil:
			err = n.webhook(ctx, *s.Webhook, e)
		}
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) desktop(ctx context.Context, e Event) error {
	if n.Desktop != nil {
		return n.Desktop(ctx, e.Title, e.Message)
	}
	if out, err := exec.CommandContext(ctx, "notify-send", notifySendArgs(e.Title, e.Message)...).CombinedOutput(); err != nil {
		return fmt.Errorf("notify-send: %v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// notifySendArgs escapes the body: notification daemons may render markup
// in it, and it can be guest text.
func notifySendArgs(title, message string) []string {
	return []string{"--app-name=curral", "--", title, html.EscapeString(message)}
}

func (n *Notifier) secret(ctx context.Context, v string) (string, error) {
	name, ok := strings.CutPrefix(v, "secret:")
	if !ok {
		return v, nil
	}
	if n.Secrets == nil {
		return "", fmt.Errorf("secret %s: no secret store", name)
	}
	s, found, err := n.Secrets.Lookup(ctx, name)
	if err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("secret %s not found", name)
	}
	return s, nil
}

func (n *Notifier) client() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	return http.DefaultClient
}

func (n *Notifier) post(ctx context.Context, sink, u, ctype string, body []byte, hdr http.Header) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := n.client().Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", sink, err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", sink, resp.Status)
	}
	return nil
}

func (n *Notifier) ntfy(ctx context.Context, s Ntfy, e Event) error {
	base := s.URL
	if base == "" {
		base = "https://ntfy.sh"
	}
	hdr := http.Header{}
	hdr.Set("Title", asciiHeader(e.Title))
	hdr.Set("Tags", e.Kind)
	if s.Token != "" {
		tok, err := n.secret(ctx, s.Token)
		if err != nil {
			return fmt.Errorf("ntfy: %w", err)
		}
		hdr.Set("Authorization", "Bearer "+tok)
	}
	return n.post(ctx, "ntfy", strings.TrimRight(base, "/")+"/"+url.PathEscape(s.Topic), "text/plain; charset=utf-8", []byte(e.Message), hdr)
}

func (n *Notifier) webhook(ctx context.Context, s Webhook, e Event) error {
	hdr := http.Header{}
	for k, v := range s.Headers {
		val, err := n.secret(ctx, v)
		if err != nil {
			return fmt.Errorf("webhook header %s: %w", k, err)
		}
		hdr.Set(k, val)
	}
	body, _ := json.Marshal(e)
	return n.post(ctx, "webhook", s.URL, "application/json", body, hdr)
}

// asciiHeader keeps a header value to printable ASCII.
func asciiHeader(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
