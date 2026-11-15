package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/internal/audit"
	"github.com/blackshieldpt/catchpen/internal/config"
	"github.com/blackshieldpt/catchpen/internal/egress"
	"github.com/blackshieldpt/catchpen/internal/template"
	"github.com/blackshieldpt/catchpen/internal/vm"
	"github.com/blackshieldpt/catchpen/pkg/api"
	"github.com/blackshieldpt/catchpen/pkg/apiclient"
)

// recordAudit writes an audit event, failing the test if it can't.
func recordAudit(t *testing.T, l audit.Log, event, vm string, detail map[string]any) {
	t.Helper()
	if err := l.Record(event, vm, detail); err != nil {
		t.Fatal(err)
	}
}

// followN follows audit events from after, until it has n of them, and
// returns them; a follow that gives fewer within 5 s fails the test.
func followN(t *testing.T, c *apiclient.Client, vm string, after time.Time, n int) []api.AuditEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(5*time.Second, cancel) // a follow that stalls is a failure, not a hang
	defer timer.Stop()
	var got []api.AuditEvent
	_ = c.AuditFollow(ctx, vm, after, func(e api.AuditEvent) error {
		got = append(got, e)
		if len(got) == n {
			cancel()
		}
		return nil
	})
	if len(got) != n {
		t.Fatalf("%d events, want %d: %+v", len(got), n, got)
	}
	return got
}

func TestAuditFollow(t *testing.T) {
	h := home(t, nil)
	log := audit.Log{Path: h.p.Audit()}
	start := time.Now().Add(-time.Minute)
	recordAudit(t, log, audit.VMStart, "alpha", map[string]any{"x": "1"})
	recordAudit(t, log, audit.VMStop, "beta", nil)
	c := h.client(t)

	evs := followN(t, c, "", start, 2)
	if evs[0].Event != audit.VMStart || evs[0].VM != "alpha" || evs[0].Detail["x"] != "1" ||
		evs[1].Event != audit.VMStop || evs[1].VM != "beta" {
		t.Errorf("events %+v: want the two, oldest first, as written", evs)
	}

	// A follow from now delivers nothing of the two already written: the first
	// thing it sends is the event recorded after it started.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan api.AuditEvent, 8)
	done := make(chan error, 1)
	go func() {
		done <- c.AuditFollow(ctx, "", time.Now(), func(e api.AuditEvent) error {
			got <- e
			return nil
		})
	}()
	select {
	case e := <-got:
		t.Fatalf("a follow from now delivered %+v before any new event", e)
	case <-time.After(500 * time.Millisecond):
	}
	recordAudit(t, log, audit.VMStart, "gamma", nil)
	select {
	case e := <-got:
		if e.VM != "gamma" {
			t.Errorf("the first event after the follow started is %+v, want gamma's", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no new event within 5 s")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow didn't return after cancel")
	}
}

func TestAuditFollowAfter(t *testing.T) {
	h := home(t, nil)
	log := audit.Log{Path: h.p.Audit()}
	start := time.Now().Add(-time.Minute)
	recordAudit(t, log, audit.VMStart, "alpha", nil)
	recordAudit(t, log, audit.VMStart, "beta", nil)
	recordAudit(t, log, audit.VMStop, "beta", nil)
	recordAudit(t, log, audit.VMStop, "alpha", nil)
	c := h.client(t)

	for _, path := range []string{api.PathAuditFollow + "?after=yesterday", api.PathAuditFollow + "?vm=Not_A_Name"} {
		if code, body := h.raw(t, path); code != 400 || !strings.Contains(body, `"code":"bad_request"`) {
			t.Errorf("GET %s: %d %s, want 400 bad_request", path, code, body)
		}
	}

	for _, e := range followN(t, c, "alpha", start, 2) {
		if e.VM != "alpha" {
			t.Errorf("a follow of alpha delivered %+v", e)
		}
	}
}

func TestAuditFollowScope(t *testing.T) {
	h := home(t, nil)
	log := audit.Log{Path: h.p.Audit()}
	recordAudit(t, log, audit.SecretSet, "alpha", map[string]any{"name": "TOKEN"})
	recordAudit(t, log, audit.ConfigWrite, "", map[string]any{"old_hash": "a", "new_hash": "b"})
	recordAudit(t, log, audit.CredsPush, "alpha", map[string]any{"secrets": []string{"S"}, "missing": []string{"M"}})
	recordAudit(t, log, audit.ForgeAdd, "", map[string]any{"token_secret": "T", "password_secret": "P"})
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	start := time.Now().Add(-time.Minute)
	laptop := a.client(t, apiclient.Options{Cert: &a.laptop}) // no scope
	cockpit := a.client(t, apiclient.Options{HMAC: &apiclient.HMAC{Name: "cockpit", Key: a.key}})

	withheld := []string{"name", "old_hash", "new_hash", "secrets", "missing", "token_secret", "password_secret"}
	for _, e := range followN(t, laptop, "", start, 4) {
		for _, k := range withheld {
			if _, ok := e.Detail[k]; ok {
				t.Errorf("laptop's follow shows %s in %s: %v", k, e.Event, e.Detail)
			}
		}
	}
	shown := map[string][]string{
		audit.SecretSet:   {"name"},
		audit.ConfigWrite: {"old_hash", "new_hash"},
		audit.CredsPush:   {"secrets", "missing"},
		audit.ForgeAdd:    {"token_secret", "password_secret"},
	}
	for _, e := range followN(t, cockpit, "", start, 4) {
		for _, k := range shown[e.Event] {
			if _, ok := e.Detail[k]; !ok {
				t.Errorf("cockpit's follow lacks %s in %s: %v", k, e.Event, e.Detail)
			}
		}
	}
}

func TestEgressLogPage(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Egress: "allowlist"})
	dir := h.p.VM("alpha")
	at := func(s int) time.Time { return time.Date(2026, 10, 1, 12, 0, s, 0, time.UTC) }
	for _, e := range []egress.LogEntry{
		{Time: at(1), VM: "alpha", Method: "GET", Host: "a.test", Port: 80, Action: egress.ActionAllow},
		{Time: at(2), VM: "alpha", Method: "GET", Host: "b.test", Port: 80, Action: egress.ActionAllow},
		{Time: at(2), VM: "alpha", Method: "GET", Host: "c.test", Port: 80, Action: egress.ActionDeny},
		{Time: at(3), VM: "alpha", Method: "GET", Host: "d.test", Port: 80, Action: egress.ActionAllow},
	} {
		if err := egress.Append(dir, e); err != nil {
			t.Fatal(err)
		}
	}
	c := h.client(t)
	ctx := context.Background()

	first, err := c.EgressLog(ctx, "alpha", 2, "")
	if err != nil || len(first.Entries) != 2 || first.Entries[0].Host != "d.test" || first.Entries[1].Host != "c.test" || first.Next == "" {
		t.Fatalf("first page: %+v, %v; want d and c, with a next", first, err)
	}
	second, err := c.EgressLog(ctx, "alpha", 2, first.Next)
	if err != nil || len(second.Entries) != 2 || second.Entries[0].Host != "b.test" || second.Entries[1].Host != "a.test" || second.Next != "" {
		t.Fatalf("second page: %+v, %v; want b and a, the last", second, err)
	}
	if all, err := c.EgressLog(ctx, "alpha", 0, ""); err != nil || len(all.Entries) != 4 || all.Next != "" {
		t.Errorf("default limit: %+v, %v; want all four", all, err)
	}

	var e *api.Error
	if _, err := c.EgressLog(ctx, "nope", 2, ""); !errors.As(err, &e) || e.Code != api.CodeNotFound {
		t.Errorf("a missing VM: %v, want not_found", err)
	}
	if _, err := c.EgressLog(ctx, "alpha", 1001, ""); !errors.As(err, &e) || e.Code != api.CodeBadRequest {
		t.Errorf("limit 1001: %v, want bad_request", err)
	}
	if _, err := c.EgressLog(ctx, "alpha", 2, "garbage"); !errors.As(err, &e) || e.Code != api.CodeBadRequest {
		t.Errorf("a garbage before: %v, want bad_request", err)
	}
}

func TestEgressLogFollow(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Egress: "allowlist"})
	dir := h.p.VM("alpha")
	c := h.client(t)
	start := time.Now().Add(-time.Minute)
	if err := egress.Append(dir, egress.LogEntry{Time: start.Add(-time.Second), VM: "alpha", Method: "GET", Host: "old.test", Port: 80, Action: egress.ActionAllow}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan api.EgressLogEntry, 8)
	done := make(chan error, 1)
	go func() {
		done <- c.EgressLogFollow(ctx, "alpha", start, func(e api.EgressLogEntry) error {
			got <- e
			return nil
		})
	}()
	if err := egress.Append(dir, egress.LogEntry{Time: time.Now(), VM: "alpha", Method: "CONNECT", Host: "new.test", Port: 443, Action: egress.ActionDeny, Reason: "not allowed"}); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-got:
		if e.Host != "new.test" || e.VM != "alpha" || e.Method != "CONNECT" || e.Port != 443 || e.Action != egress.ActionDeny || e.Reason != "not allowed" {
			t.Errorf("entry %+v, want the new one as written", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no new entry within 5 s")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the follow didn't return after cancel")
	}
}

func TestEgressLogFollowErrors(t *testing.T) {
	h := home(t, nil)
	h.writeVM(t, vm.State{Name: "alpha", Template: "base", Created: created, Egress: "allowlist"})
	c := h.client(t)

	if code, body := h.raw(t, api.PathEgressLogFollow("alpha")+"?after=yesterday"); code != 400 || !strings.Contains(body, `"code":"bad_request"`) {
		t.Errorf("a bad after: %d %s, want 400 bad_request", code, body)
	}
	if code, body := h.raw(t, api.PathEgressLogFollow("nope")+"?after=yesterday"); code != 404 || !strings.Contains(body, `"code":"not_found"`) {
		t.Errorf("a missing VM with a bad after: %d %s, want 404 not_found (the VM is looked up first)", code, body)
	}
	var e *api.Error
	err := c.EgressLogFollow(context.Background(), "nope", time.Time{}, func(api.EgressLogEntry) error { return nil })
	if !errors.As(err, &e) || e.Code != api.CodeNotFound {
		t.Errorf("a missing VM: %v, want not_found", err)
	}
}

// TestAuditFollowOutlivesAuthDeadline proves a follow is not cut off by the
// write deadline authTCP sets when a TCP request is authenticated: with a
// short requestTimeout, an event written after it must still arrive.
func TestAuditFollowOutlivesAuthDeadline(t *testing.T) {
	oldRT := requestTimeout
	requestTimeout = 300 * time.Millisecond
	t.Cleanup(func() { requestTimeout = oldRT }) // registered first: runs last

	h := home(t, nil)
	a := serveTCPManager(t, func() (*vm.Manager, error) {
		return &vm.Manager{P: h.p, Cfg: config.Default(), Tpl: template.Loader{UserDir: h.p.Templates()}}, nil
	})
	c := a.client(t, apiclient.Options{Cert: &a.laptop})
	got := make(chan api.AuditEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.AuditFollow(ctx, "", time.Time{}, func(e api.AuditEvent) error { got <- e; return nil })
	}()
	time.Sleep(600 * time.Millisecond) // past requestTimeout
	recordAudit(t, audit.Log{Path: h.p.Audit()}, audit.VMStart, "alpha", nil)
	select {
	case e := <-got:
		if e.Event != audit.VMStart || e.VM != "alpha" {
			t.Errorf("event = %+v, want vm.start for alpha", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event after the auth deadline: the follow was cut off")
	}
	cancel()
	<-done
}
