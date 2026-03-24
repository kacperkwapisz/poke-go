// Package auth handles poke credentials: login (OAuth browser flow),
// logout, and loading the stored token from ~/.config/poke/credentials.json.
package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

const (
	pokeBaseURL = "https://poke.com"
	credsSubDir = ".config/poke"
	credsFile   = "credentials.json"
)

// Credentials is the structure stored in ~/.config/poke/credentials.json.
type Credentials struct {
	Token string `json:"token"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// CredentialsPath returns the absolute path to the credentials file.
func CredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine home directory: %w", err)
	}
	return filepath.Join(home, credsSubDir, credsFile), nil
}

// Load reads and returns the stored credentials.
// Returns a friendly error if the user is not logged in.
func Load() (*Credentials, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not logged in — run 'poke login' first")
		}
		return nil, fmt.Errorf("reading credentials: %w", err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("invalid credentials file: %w", err)
	}
	if creds.Token == "" {
		return nil, fmt.Errorf("not logged in — run 'poke login' first")
	}
	return &creds, nil
}

func save(creds *Credentials) error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}
	return nil
}

// Login starts a local HTTP callback server, opens the browser to the
// poke.com CLI auth page, waits for the token redirect, and saves
// credentials to disk.
func Login() error {
	// Start a local server on a random port to receive the OAuth callback.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("cannot start local server: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Generate a random state value to prevent CSRF.
	stateBytes := make([]byte, 16)
	if _, err := rand.Read(stateBytes); err != nil {
		listener.Close()
		return err
	}
	state := hex.EncodeToString(stateBytes)

	authURL := fmt.Sprintf("%s/auth/cli?port=%d&state=%s", pokeBaseURL, port, state)
	fmt.Printf("Opening %s\n", authURL)
	fmt.Println("Waiting for authentication...")
	openBrowser(authURL)

	tokenCh := make(chan string, 1)
	errCh := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			errCh <- fmt.Errorf("auth state mismatch")
			return
		}
		token := r.URL.Query().Get("token")
		if token == "" {
			http.Error(w, "missing token", http.StatusBadRequest)
			errCh <- fmt.Errorf("no token in callback")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body style="font-family:sans-serif;text-align:center;padding:60px">`+
			`<h2>Logged in. You can close this tab.</h2></body></html>`)
		tokenCh <- token
	})

	srv := &http.Server{Handler: mux}
	go func() {
		_ = srv.Serve(listener)
	}()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	select {
	case token := <-tokenCh:
		creds := &Credentials{Token: token}
		if err := save(creds); err != nil {
			return err
		}
		fmt.Println("Logged in.")
		return nil
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return fmt.Errorf("login timed out after 5 minutes")
	}
}

// Logout removes the stored credentials file.
func Logout() error {
	path, err := CredentialsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			fmt.Println("Not logged in.")
			return nil
		}
		return fmt.Errorf("removing credentials: %w", err)
	}
	fmt.Println("Logged out.")
	return nil
}

// openBrowser opens url in the default system browser.
func openBrowser(url string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{url}
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler", url}
	default:
		cmd, args = "xdg-open", []string{url}
	}
	_ = exec.Command(cmd, args...).Start()
}
