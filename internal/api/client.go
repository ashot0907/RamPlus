package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Client — клиент API демона через unix-сокет.
type Client struct {
	Socket string
	HTTP   *http.Client
}

// NewClient — клиент для сокета path.
func NewClient(path string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}
	return &Client{Socket: path, HTTP: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// Get — GET path, ответ 200 декодируется в v; иначе — *Error.
func (c *Client) Get(ctx context.Context, path string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ramplusd"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e Error
		if json.NewDecoder(resp.Body).Decode(&e) != nil || e.Code == "" {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return &e
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// Health — GET /v1/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.Get(ctx, "/v1/health", &h)
	return h, err
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
