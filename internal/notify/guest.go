package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// GuestRequest is what agent hooks in the guest POST to /notify. Claude
// Code's hook payload (hook_event_name, message) is accepted as is.
type GuestRequest struct {
	Agent   string `json:"agent"`
	Hook    string `json:"hook_event_name"`
	Message string `json:"message"`
}

const maxGuestBody = 8 << 10

// ServeGuest answers one HTTP request from a guest on conn and, if it is a
// valid notification, delivers it with the notifier from get. Every
// connection takes a rate-limit token first, so a guest can't make the host
// do (or log) more than the limit allows. Nothing the host knows (sink URLs,
// tokens, errors) is sent back to the guest.
func ServeGuest(ctx context.Context, conn net.Conn, vm string, lim Limiter, get func() (*Notifier, error)) error {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	reply := func(code int) {
		fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Length: 0\r\nConnection: close\r\n\r\n", code, http.StatusText(code))
	}
	if ok, err := lim.Allow(time.Now()); err != nil || !ok {
		reply(http.StatusTooManyRequests)
		return err
	}
	req, err := http.ReadRequest(bufio.NewReader(io.LimitReader(conn, 16<<10)))
	if err != nil {
		reply(http.StatusBadRequest)
		return err
	}
	if req.Method != http.MethodPost || req.URL.Path != "/notify" {
		reply(http.StatusNotFound)
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, maxGuestBody+1))
	if err != nil || len(body) > maxGuestBody {
		reply(http.StatusRequestEntityTooLarge)
		return err
	}
	var g GuestRequest
	if err := json.Unmarshal(body, &g); err != nil {
		reply(http.StatusBadRequest)
		return nil
	}
	reply(http.StatusAccepted)
	conn.Close() // don't keep the guest's hook waiting on delivery
	if g.Agent == "" {
		g.Agent = req.URL.Query().Get("agent")
	}
	agent := Clean(g.Agent, 20)
	if agent == "" {
		agent = "agent"
	}
	msg := g.Message
	if msg == "" {
		msg = "finished its turn" // Stop, session.idle, agent_settled, agent_end
	}
	n, err := get()
	if err != nil {
		return err
	}
	return n.Send(ctx, Event{Kind: AgentWaiting, VM: vm, Title: "curral " + vm + ": " + agent + " is waiting", Message: msg})
}
