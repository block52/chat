// Package ws exposes the chat hub over WebSocket. Each connection joins exactly
// one table (chosen via the ?table= query param). Inbound frames must be signed
// chat messages; the server verifies the Cosmos signature before broadcasting.
package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/block52/chat/internal/auth"
	"github.com/block52/chat/internal/hub"
)

// Options configure the handler.
type Options struct {
	Hub *hub.Hub
	// AllowedOrigins is passed to the websocket accept check. Empty => same-origin.
	AllowedOrigins []string
	// ExpectedHRP, if set, requires the signer address to use this bech32 prefix
	// (e.g. "cosmos"). Empty => any prefix accepted.
	ExpectedHRP string
	// MaxMessageBytes rejects oversized frames.
	MaxMessageBytes int
	Logger          *slog.Logger
}

// inbound is the frame a client sends: a signed message whose signed payload is
// the Text being chatted, scoped to a Table.
type inbound struct {
	auth.SignedMessage
	// Text is the chat content. It MUST equal the signed Message, otherwise the
	// signature would not cover what is displayed.
	Text string `json:"text"`
}

// outbound is what the server pushes to clients.
type outbound struct {
	Type    string `json:"type"` // "message" | "error" | "backlog"
	Table   string `json:"table,omitempty"`
	Address string `json:"address,omitempty"`
	Text    string `json:"text,omitempty"`
	TS      int64  `json:"ts,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Handler returns an http.Handler serving the chat WebSocket endpoint.
func Handler(opts Options) http.Handler {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.MaxMessageBytes <= 0 {
		opts.MaxMessageBytes = 4096
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		table := strings.TrimSpace(r.URL.Query().Get("table"))
		if table == "" {
			http.Error(w, "missing ?table=", http.StatusBadRequest)
			return
		}

		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: opts.AllowedOrigins,
		})
		if err != nil {
			opts.Logger.Debug("ws accept failed", "err", err)
			return
		}
		defer c.CloseNow() //nolint:errcheck

		c.SetReadLimit(int64(opts.MaxMessageBytes))
		serve(r.Context(), c, table, opts)
	})
}

func serve(ctx context.Context, c *websocket.Conn, table string, opts Options) {
	sub, backlog := opts.Hub.Subscribe(table)
	defer opts.Hub.Unsubscribe(table, sub)

	// Fan broadcasts out to this socket in a dedicated goroutine so reads and
	// writes don't block each other.
	writerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		// Replay recent backlog first.
		for _, m := range backlog {
			if writeMsg(writerCtx, c, m) != nil {
				cancel()
				return
			}
		}
		for {
			select {
			case <-writerCtx.Done():
				return
			case m, ok := <-sub.C():
				if !ok {
					return
				}
				if writeMsg(writerCtx, c, m) != nil {
					cancel()
					return
				}
			}
		}
	}()

	for {
		var in inbound
		err := wsjson.Read(ctx, c, &in)
		if err != nil {
			var ce websocket.CloseError
			if !errors.As(err, &ce) && !errors.Is(err, context.Canceled) {
				opts.Logger.Debug("ws read ended", "err", err)
			}
			return
		}
		if err := handleInbound(ctx, c, table, in, opts); err != nil {
			return
		}
	}
}

// handleInbound verifies and publishes one inbound frame. It returns an error
// only when the connection should be torn down.
func handleInbound(ctx context.Context, c *websocket.Conn, table string, in inbound, opts Options) error {
	// The signed payload must be exactly the text we will broadcast, so a signature
	// can't be lifted from one message and reused to display different content.
	if in.Message != in.Text {
		return writeErr(ctx, c, "signed message does not match text")
	}
	hrp, err := auth.Verify(in.SignedMessage)
	if err != nil {
		opts.Logger.Debug("signature rejected", "addr", in.Address, "err", err)
		return writeErr(ctx, c, "signature verification failed")
	}
	if opts.ExpectedHRP != "" && hrp != opts.ExpectedHRP {
		return writeErr(ctx, c, "unexpected address prefix")
	}

	opts.Hub.Publish(hub.Message{
		Table:   table,
		Address: in.Address,
		Text:    in.Text,
		UnixMs:  time.Now().UnixMilli(),
	})
	return nil
}

func writeMsg(ctx context.Context, c *websocket.Conn, m hub.Message) error {
	return wsjson.Write(ctx, c, outbound{
		Type: "message", Table: m.Table, Address: m.Address, Text: m.Text, TS: m.UnixMs,
	})
}

func writeErr(ctx context.Context, c *websocket.Conn, msg string) error {
	// Errors are advisory; marshaling never fails for this fixed shape.
	b, _ := json.Marshal(outbound{Type: "error", Error: msg})
	return c.Write(ctx, websocket.MessageText, b)
}
