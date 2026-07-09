package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HTTPTransport reaches the Supervisor REST API directly over http://supervisor
// with the add-on's injected SUPERVISOR_TOKEN. This is the transport the agent
// add-on uses (it runs inside HAOS with hassio_api + hassio_role: manager), as
// opposed to the CLI's WS-via-Core transport — both drive the same operations in
// this package.
type HTTPTransport struct {
	// BaseURL is the Supervisor root, e.g. "http://supervisor".
	BaseURL string
	// Token is the SUPERVISOR_TOKEN the Supervisor injects into the add-on.
	Token string
	// HTTP is the client used for calls; a nil client defaults to http.Client{}.
	HTTP *http.Client
}

// Call implements Transport. Supervisor wraps every response in an envelope
// ({"result":"ok","data":...} / {"result":"error","message":...}); this unwraps
// it to the bare `data` so the result matches the WS transport, and maps a
// Supervisor rejection (result=error or a 4xx) to *Error.
func (t *HTTPTransport) Call(ctx context.Context, method, endpoint string, payload any, timeout time.Duration) (json.RawMessage, error) {
	callCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		callCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, fmt.Errorf("supervisor %s %s: encode: %w", method, endpoint, err)
		}
		body = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(callCtx, methodUpper(method), t.BaseURL+endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("supervisor %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+t.Token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := t.HTTP
	if client == nil {
		client = &http.Client{}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("supervisor %s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("supervisor %s %s: read: %w", method, endpoint, err)
	}

	var env struct {
		Result  string          `json:"result"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	// A non-JSON body (rare Supervisor 5xx) still surfaces as a Supervisor error.
	_ = json.Unmarshal(raw, &env)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || env.Result == "error" {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}

		return nil, &Error{Method: method, Endpoint: endpoint, Message: msg}
	}

	return env.Data, nil
}

func methodUpper(m string) string {
	switch m {
	case "get", "GET":
		return http.MethodGet
	case "post", "POST":
		return http.MethodPost
	default:
		return http.MethodGet
	}
}
