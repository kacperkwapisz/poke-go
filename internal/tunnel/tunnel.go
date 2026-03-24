// Package tunnel implements the 'poke tunnel' command.
// It registers a named tunnel with the Poke API, then maintains a WebSocket
// connection that proxies inbound HTTP requests from tunnel.poke.com to a
// local server URL.
package tunnel

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kacperkwapisz/poke-go/internal/auth"
	"github.com/kacperkwapisz/poke-go/internal/ws"
)

const (
	pokeAPI   = "https://poke.com/api/v1"
	tunnelWSS = "wss://tunnel.poke.com/ws"
)

// --- API types ---

type registerRequest struct {
	LocalURL string `json:"local_url"`
	Name     string `json:"name,omitempty"`
}

type registerResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// proxyEnvelope is the message format over the WebSocket tunnel.
// The server sends requests; the client sends responses.
type proxyEnvelope struct {
	// Request fields (server → client)
	RequestID string            `json:"request_id,omitempty"`
	Method    string            `json:"method,omitempty"`
	Path      string            `json:"path,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Body      string            `json:"body,omitempty"` // base64-encoded

	// Response fields (client → server)
	Status int `json:"status,omitempty"`
}

// Run parses arguments and starts the tunnel.
func Run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: poke tunnel <url> [--name <name>]")
	}

	localURL := args[0]
	name := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--name" && i+1 < len(args) {
			name = args[i+1]
			i++
		}
	}

	creds, err := auth.Load()
	if err != nil {
		return err
	}

	// Register the tunnel with the Poke API.
	tunnelID, tunnelURL, err := registerTunnel(creds.Token, localURL, name)
	if err != nil {
		return fmt.Errorf("creating tunnel: %w", err)
	}

	fmt.Printf("Tunnel:  %s\n", tunnelURL)
	fmt.Printf("Local:   %s\n", localURL)
	fmt.Println("Ready. Press Ctrl+C to stop.")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Println("\nDisconnecting...")
		cancel()
	}()

	return runProxy(ctx, creds.Token, tunnelID, localURL)
}

// registerTunnel calls the Poke API to allocate a tunnel and returns its ID and URL.
func registerTunnel(token, localURL, name string) (id, tunnelURL string, err error) {
	body, _ := json.Marshal(registerRequest{LocalURL: localURL, Name: name})
	req, err := http.NewRequest(http.MethodPost, pokeAPI+"/tunnels", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("network error: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		// ok
	case http.StatusUnauthorized:
		return "", "", fmt.Errorf("unauthorized — run 'poke login' again")
	default:
		b, _ := io.ReadAll(resp.Body)
		return "", "", fmt.Errorf("server error %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var r registerResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", fmt.Errorf("decoding response: %w", err)
	}
	return r.ID, r.URL, nil
}

// runProxy connects to the Poke tunnel WebSocket and proxies requests to
// localURL until ctx is cancelled or the connection closes.
func runProxy(ctx context.Context, token, tunnelID, localURL string) error {
	headers := http.Header{
		"Authorization": {"Bearer " + token},
	}
	wsURL := fmt.Sprintf("%s/%s", tunnelWSS, tunnelID)

	backoff := 2 * time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return nil
		}

		conn, err := ws.Dial(wsURL, headers)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connection error: %v (retrying in %s)\n", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
				continue
			}
		}
		backoff = 2 * time.Second // reset on successful connect

		err = serveConn(ctx, conn, localURL)
		conn.Close()

		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "tunnel closed: %v (reconnecting in %s)\n", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
				backoff = min(backoff*2, maxBackoff)
			}
		}
	}
}

// serveConn reads proxy requests from the WebSocket and dispatches them to
// the local server, writing back responses on the same connection.
func serveConn(ctx context.Context, conn *ws.Conn, localURL string) error {
	readErr := make(chan error, 1)

	go func() {
		for {
			msg, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			go dispatch(conn, msg, localURL)
		}
	}()

	select {
	case <-ctx.Done():
		return nil
	case err := <-readErr:
		if err == io.EOF {
			return fmt.Errorf("server closed connection")
		}
		return err
	}
}

// dispatch processes one proxied request and sends the response back over
// the WebSocket.
func dispatch(conn *ws.Conn, msg []byte, localURL string) {
	var env proxyEnvelope
	if err := json.Unmarshal(msg, &env); err != nil {
		return
	}
	if env.RequestID == "" || env.Method == "" {
		return
	}

	// Decode body if present.
	var bodyReader io.Reader
	if env.Body != "" {
		decoded, err := base64.StdEncoding.DecodeString(env.Body)
		if err != nil {
			sendError(conn, env.RequestID, http.StatusBadRequest)
			return
		}
		bodyReader = bytes.NewReader(decoded)
	}

	// Build the upstream request.
	target := strings.TrimRight(localURL, "/") + env.Path
	req, err := http.NewRequest(env.Method, target, bodyReader)
	if err != nil {
		sendError(conn, env.RequestID, http.StatusBadRequest)
		return
	}
	for k, v := range env.Headers {
		req.Header.Set(k, v)
	}

	// Forward to local server.
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sendError(conn, env.RequestID, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	respHeaders := make(map[string]string, len(resp.Header))
	for k, vs := range resp.Header {
		respHeaders[k] = vs[0]
	}

	response := proxyEnvelope{
		RequestID: env.RequestID,
		Status:    resp.StatusCode,
		Headers:   respHeaders,
		Body:      base64.StdEncoding.EncodeToString(respBody),
	}
	out, _ := json.Marshal(response)
	_ = conn.WriteMessage(out)
}

func sendError(conn *ws.Conn, requestID string, status int) {
	env := proxyEnvelope{RequestID: requestID, Status: status}
	if out, err := json.Marshal(env); err == nil {
		_ = conn.WriteMessage(out)
	}
}

func min(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
