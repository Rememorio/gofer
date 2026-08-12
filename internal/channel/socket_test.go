package channel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCoderProviderSocketRoundTrip(t *testing.T) {
	t.Parallel()
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("Accept = %v", err)
			return
		}
		defer func() { _ = connection.CloseNow() }()
		if err = connection.Write(request.Context(), websocket.MessageText, []byte("hello")); err != nil {
			t.Errorf("Write = %v", err)
			return
		}
		_, payload, err := connection.Read(request.Context())
		if err != nil {
			t.Errorf("Read = %v", err)
			return
		}
		received <- string(payload)
		_ = connection.Close(websocket.StatusNormalClosure, "done")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	socket, err := dialProviderSocket(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), server.Client())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := socket.Read(ctx)
	if err != nil || string(payload) != "hello" {
		t.Fatalf("Read = %q, %v", payload, err)
	}
	if err = socket.Write(ctx, []byte("world")); err != nil {
		t.Fatal(err)
	}
	select {
	case value := <-received:
		if value != "world" {
			t.Fatalf("received = %q", value)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err = socket.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestProviderSocketDialFailure(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := dialProviderSocket(ctx, "ws://127.0.0.1:1/?ticket=secret", http.DefaultClient); err == nil {
		t.Fatal("dial failure was accepted")
	} else if strings.Contains(err.Error(), "ticket=secret") {
		t.Fatalf("dial error leaked socket ticket: %v", err)
	}
}
