package channel

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
)

const providerSocketReadLimit = 2 << 20

type providerSocket interface {
	Read(context.Context) ([]byte, error)
	Write(context.Context, []byte) error
	Close() error
}

type providerSocketDialer func(context.Context, string, *http.Client) (providerSocket, error)

type coderSocket struct{ connection *websocket.Conn }

func dialProviderSocket(ctx context.Context, endpoint string, client *http.Client) (providerSocket, error) {
	connection, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, sanitizeProviderNetworkError(err)
	}
	connection.SetReadLimit(providerSocketReadLimit)
	return coderSocket{connection: connection}, nil
}

func (socket coderSocket) Read(ctx context.Context) ([]byte, error) {
	_, payload, err := socket.connection.Read(ctx)
	return payload, err
}

func (socket coderSocket) Write(ctx context.Context, payload []byte) error {
	return socket.connection.Write(ctx, websocket.MessageText, payload)
}

func (socket coderSocket) Close() error {
	return socket.connection.Close(websocket.StatusNormalClosure, "provider stopped")
}
