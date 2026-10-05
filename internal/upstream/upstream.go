// Package upstream abstracts outbound HTTP so production code uses CLIProxyAPI
// host callbacks while tests use net/http against fake servers.
package upstream

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// Request is an outbound HTTP request. CallbackID ties the request to the
// lifetime of the inbound CLIProxyAPI request when non-empty.
type Request struct {
	Method     string
	URL        string
	Headers    http.Header
	Body       []byte
	CallbackID string
}

// Response is a buffered HTTP response.
type Response struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// Stream is an open streaming HTTP response.
type Stream struct {
	StatusCode int
	Headers    http.Header
	ID         string
}

// Transport performs outbound HTTP. Implementations must honor ctx
// cancellation for Do, OpenStream, and Read.
type Transport interface {
	Do(ctx context.Context, req Request) (Response, error)
	OpenStream(ctx context.Context, req Request) (Stream, error)
	// Read returns the next payload chunk. done is true once the body is
	// exhausted (payload may still carry final bytes).
	Read(ctx context.Context, streamID string) (payload []byte, done bool, err error)
	Close(streamID string)
}

// NetHTTP is a Transport backed by net/http, used by tests and local tools.
type NetHTTP struct {
	Client  *http.Client
	mu      sync.Mutex
	streams map[string]io.ReadCloser
	next    atomic.Uint64
}

// NewNetHTTP returns a NetHTTP transport.
func NewNetHTTP(client *http.Client) *NetHTTP {
	if client == nil {
		client = http.DefaultClient
	}
	return &NetHTTP{Client: client, streams: map[string]io.ReadCloser{}}
}

func (t *NetHTTP) newRequest(ctx context.Context, req Request) (*http.Request, error) {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, req.URL, bytes.NewReader(req.Body))
	if err != nil {
		return nil, err
	}
	for k, v := range req.Headers {
		httpReq.Header[k] = append([]string(nil), v...)
	}
	return httpReq, nil
}

// Do implements Transport.
func (t *NetHTTP) Do(ctx context.Context, req Request) (Response, error) {
	httpReq, err := t.newRequest(ctx, req)
	if err != nil {
		return Response{}, err
	}
	resp, err := t.Client.Do(httpReq)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Response{}, err
	}
	return Response{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), Body: body}, nil
}

// OpenStream implements Transport.
func (t *NetHTTP) OpenStream(ctx context.Context, req Request) (Stream, error) {
	httpReq, err := t.newRequest(ctx, req)
	if err != nil {
		return Stream{}, err
	}
	resp, err := t.Client.Do(httpReq)
	if err != nil {
		return Stream{}, err
	}
	id := fmt.Sprintf("s%d", t.next.Add(1))
	t.mu.Lock()
	t.streams[id] = resp.Body
	t.mu.Unlock()
	return Stream{StatusCode: resp.StatusCode, Headers: resp.Header.Clone(), ID: id}, nil
}

// Read implements Transport.
func (t *NetHTTP) Read(ctx context.Context, streamID string) ([]byte, bool, error) {
	t.mu.Lock()
	body := t.streams[streamID]
	t.mu.Unlock()
	if body == nil {
		return nil, true, fmt.Errorf("stream %s is not open", streamID)
	}
	if err := ctx.Err(); err != nil {
		t.Close(streamID)
		return nil, true, err
	}
	buf := make([]byte, 16*1024)
	n, err := body.Read(buf)
	if err == io.EOF {
		t.Close(streamID)
		return buf[:n], true, nil
	}
	if err != nil {
		t.Close(streamID)
		return buf[:n], true, err
	}
	return buf[:n], false, nil
}

// Close implements Transport.
func (t *NetHTTP) Close(streamID string) {
	t.mu.Lock()
	body := t.streams[streamID]
	delete(t.streams, streamID)
	t.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
}

// OpenStreams reports how many streams are still open (for tests).
func (t *NetHTTP) OpenStreams() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.streams)
}
