//go:build integration && realtime

package postgres

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	centrifuge "github.com/centrifugal/centrifuge-go"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type forumPublication struct {
	ID       string          `json:"id"`
	Version  int             `json:"version"`
	Type     string          `json:"type"`
	ScopeID  string          `json:"scope_id"`
	Position int64           `json:"position,string"`
	Data     json.RawMessage `json:"data"`
}

// Validate the persisted envelope received over WSS, rather than trusting adapter comments.
func decodeForumPublication(t *testing.T, publication centrifuge.PublicationEvent, kind, scopeID string) forumPublication {
	t.Helper()
	var event forumPublication
	if err := json.Unmarshal(publication.Data, &event); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(event.ID)
	if err != nil || id == uuid.Nil || id.String() != event.ID {
		t.Fatal("publication has no canonical stable event ID")
	}
	if event.Version != 1 || event.Type != kind || event.ScopeID != scopeID || event.Position < 1 {
		t.Fatal("publication envelope violates event contract")
	}
	return event
}

func connectRawRealtime(t *testing.T, ctx context.Context, address string, tlsConfig *tls.Config, token string) *websocket.Conn {
	t.Helper()
	connection, response, err := (&websocket.Dialer{TLSClientConfig: tlsConfig, HandshakeTimeout: time.Second}).DialContext(ctx, address, http.Header{})
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	if err := connection.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := connection.WriteJSON(map[string]any{"id": 1, "connect": map[string]any{"token": token}}); err != nil {
		t.Fatal(err)
	}
	readRawRealtimeReply(t, connection, "connect")
	return connection
}

func readRawRealtimeReply(t *testing.T, connection *websocket.Conn, kind string) {
	t.Helper()
	var reply map[string]json.RawMessage
	if err := connection.ReadJSON(&reply); err != nil {
		t.Fatal(err)
	}
	if reply[kind] == nil || reply["error"] != nil {
		t.Fatalf("raw WSS %s was not accepted", kind)
	}
}

func readRawRealtimeDisconnect(t *testing.T, connection *websocket.Conn, code int, reason string) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	for {
		var reply struct {
			Subscribe json.RawMessage `json:"subscribe"`
			Push      struct {
				Publication json.RawMessage `json:"pub"`
				Disconnect  *struct {
					Code   int    `json:"code"`
					Reason string `json:"reason"`
				} `json:"disconnect"`
			} `json:"push"`
		}
		err := connection.ReadJSON(&reply)
		var closed *websocket.CloseError
		if errors.As(err, &closed) && closed.Code == code && strings.Contains(closed.Text, reason) {
			return
		}
		if err != nil {
			t.Fatalf("server did not enforce expected disconnect %d: %v", code, err)
		}
		if reply.Subscribe != nil || reply.Push.Publication != nil {
			t.Fatal("late subscription or publication accepted after revocation")
		}
		if reply.Push.Disconnect != nil && reply.Push.Disconnect.Code == code && strings.Contains(reply.Push.Disconnect.Reason, reason) {
			return
		}
	}
}

func consumerPermissionFailures(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "permission denied for table realtime_outbox")
}

func waitConsumerPermissionFailures(t *testing.T, ctx context.Context, path string, minimum int) {
	t.Helper()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(8 * time.Second)
	defer timeout.Stop()
	for {
		if consumerPermissionFailures(t, path) >= minimum {
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timeout.C:
			t.Fatal("consumer did not actually retry the injected database failure")
		}
	}
}
