package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// TestFollowClient reads a follow whose stream fails after its first line: the
// event is passed to fn, and the error line comes back as the call's error.
func TestFollowClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathAuditFollow, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", api.ContentTypeNDJSON)
		_ = json.NewEncoder(w).Encode(api.AuditEvent{Time: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), Event: "vm.start", VM: "a"})
		_ = json.NewEncoder(w).Encode(api.ErrorResponse{Error: &api.Error{Code: api.CodeInternal, Message: "the audit log can't be followed"}})
	})
	l, err := net.Listen("unix", filepath.Join(t.TempDir(), "api.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go http.Serve(l, mux)
	t.Cleanup(func() { l.Close() })
	c, err := New(Options{Socket: l.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}

	var got []api.AuditEvent
	err = c.AuditFollow(context.Background(), "", time.Time{}, func(e api.AuditEvent) error {
		got = append(got, e)
		return nil
	})
	if len(got) != 1 || got[0].Event != "vm.start" || got[0].VM != "a" {
		t.Errorf("events %+v, want the one line before the error", got)
	}
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeInternal {
		t.Errorf("err %v, want the error line as an *api.Error", err)
	}
}
