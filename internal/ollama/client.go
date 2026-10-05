package ollama

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/upstream"
)

const maxErrorDetail = 500

// HTTPError is a non-2xx Ollama response.
type HTTPError struct {
	StatusCode int
	Message    string
}

func (e *HTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("ollama returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("ollama returned HTTP %d: %s", e.StatusCode, e.Message)
}

// Client talks to one Ollama instance. It never calls model-mutating endpoints
// (pull, create, copy, delete, push).
type Client struct {
	BaseURL   string
	APIKey    string
	Transport upstream.Transport
}

func (c Client) headers(withBody bool) http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	if withBody {
		h.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		h.Set("Authorization", "Bearer "+c.APIKey)
	}
	return h
}

func (c Client) url(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

func (c Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.Transport.Do(ctx, upstream.Request{Method: http.MethodGet, URL: c.url(path), Headers: c.headers(false)})
	if err != nil {
		return err
	}
	return decodeJSON(resp, out)
}

func (c Client) postJSON(ctx context.Context, path string, body any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	resp, err := c.Transport.Do(ctx, upstream.Request{Method: http.MethodPost, URL: c.url(path), Headers: c.headers(true), Body: raw})
	if err != nil {
		return err
	}
	return decodeJSON(resp, out)
}

func decodeJSON(resp upstream.Response, out any) error {
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ErrorFromBody(resp.StatusCode, resp.Body)
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return fmt.Errorf("decode ollama response: %w", err)
	}
	return nil
}

// ErrorFromBody builds an HTTPError from an Ollama error body.
func ErrorFromBody(status int, body []byte) *HTTPError {
	msg := ""
	var eb ErrorBody
	if json.Unmarshal(body, &eb) == nil && eb.Error != "" {
		msg = eb.Error
	} else {
		msg = strings.TrimSpace(string(body))
	}
	if len(msg) > maxErrorDetail {
		msg = msg[:maxErrorDetail] + "…"
	}
	return &HTTPError{StatusCode: status, Message: msg}
}

// Tags lists installed models.
func (c Client) Tags(ctx context.Context) (TagsResponse, error) {
	var out TagsResponse
	err := c.getJSON(ctx, "/api/tags", &out)
	return out, err
}

// Show returns model details.
func (c Client) Show(ctx context.Context, name string) (ShowResponse, error) {
	var out ShowResponse
	err := c.postJSON(ctx, "/api/show", map[string]any{"model": name}, &out)
	return out, err
}

// PS lists loaded runners.
func (c Client) PS(ctx context.Context) (PSResponse, error) {
	var out PSResponse
	err := c.getJSON(ctx, "/api/ps", &out)
	return out, err
}

// ChatRequestFor builds the transport request for /api/chat.
func (c Client) ChatRequestFor(body []byte, callbackID string) upstream.Request {
	h := c.headers(true)
	return upstream.Request{Method: http.MethodPost, URL: c.url("/api/chat"), Headers: h, Body: body, CallbackID: callbackID}
}
