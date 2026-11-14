package apiclient

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/websocket"

	"github.com/blackshieldpt/catchpen/pkg/api"
)

// Prompts listens for egress prompts and answers them with answer, until ctx
// is done or the server closes the stream. answer is called for every
// message: for an ask (Type api.PromptAsk), return one of the api.Decision*
// values, or "" to leave it for another client; for a done (Type
// api.PromptDone), return "": its Error says an answer was refused or the
// prompt expired, and its Decision says an answer won. An invalid decision is
// an error.
func (c *Client) Prompts(ctx context.Context, answer func(api.Prompt) string) error {
	conn, err := c.stream(ctx, api.PathEgressPrompts, nil)
	if err != nil {
		return err
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	for {
		typ, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg api.Prompt
		if json.Unmarshal(b, &msg) != nil {
			continue
		}
		d := answer(msg) // a done comes here too: answer returns "" for it
		if msg.Type != api.PromptAsk || d == "" {
			continue
		}
		if !api.ValidDecision(d) {
			return fmt.Errorf("apiclient: answer returned invalid decision %q", d)
		}
		out, err := json.Marshal(api.Prompt{Type: api.PromptAnswer, ID: msg.ID, Decision: d})
		if err != nil {
			return err
		}
		if err := conn.Write(ctx, websocket.MessageText, out); err != nil {
			return err
		}
	}
}
