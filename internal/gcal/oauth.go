package gcal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/jiyeol-lee/cli/internal/xdg"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
)

const (
	oauthCallbackAddress = "localhost:8000"
	oauthCallbackPath    = "/callback"
	oauthCallbackURL     = "http://" + oauthCallbackAddress + oauthCallbackPath
)

type OAuth struct {
	TokenPath string
	Stdout    io.Writer
	Timeout   time.Duration
	OpenURL   func(string) error
}

func (o OAuth) Client(ctx context.Context) (*http.Client, error) {
	clientID, clientSecret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET")
	if clientID == "" {
		return nil, fmt.Errorf("GOOGLE_CLIENT_ID is not set")
	}
	if clientSecret == "" {
		return nil, fmt.Errorf("GOOGLE_CLIENT_SECRET is not set")
	}
	config := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		RedirectURL:  oauthCallbackURL,
		Scopes:       []string{calendar.CalendarReadonlyScope},
	}
	token, err := loadToken(o.TokenPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("read Google OAuth token: %w", err)
		}
		token, err = o.authorize(ctx, config)
		if err != nil {
			return nil, err
		}
		if err := saveToken(o.TokenPath, token); err != nil {
			return nil, err
		}
	}
	source := &savingTokenSource{
		source:  config.TokenSource(ctx, token),
		path:    o.TokenPath,
		current: token,
		authorize: func() (*oauth2.Token, error) {
			return o.authorize(ctx, config)
		},
		replacementSource: func(token *oauth2.Token) oauth2.TokenSource {
			return config.TokenSource(ctx, token)
		},
	}
	return oauth2.NewClient(ctx, oauth2.ReuseTokenSource(token, source)), nil
}

type callbackResult struct {
	code string
	err  error
}

func (o OAuth) authorize(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", oauthCallbackAddress)
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback on %s: %w", oauthCallbackAddress, err)
	}
	defer listener.Close()
	state, err := randomState()
	if err != nil {
		return nil, err
	}
	verifier := oauth2.GenerateVerifier()
	authURL := authorizationURL(config, state, verifier)
	resultCh := make(chan callbackResult, 1)
	mux := http.NewServeMux()
	mux.Handle(oauthCallbackPath, oauthCallbackHandler(state, resultCh))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	out := o.Stdout
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "Open this URL to authorize Google Calendar:\n%s\n", authURL)
	opener := o.OpenURL
	if opener == nil {
		opener = openBrowser
	}
	_ = opener(authURL)
	timeout := o.Timeout
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	var result callbackResult
	select {
	case <-ctx.Done():
		result.err = ctx.Err()
	case <-time.After(timeout):
		result.err = fmt.Errorf("Google OAuth callback timed out")
	case result = <-resultCh:
	case err := <-serveErr:
		result.err = fmt.Errorf("OAuth callback server: %w", err)
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	if result.err != nil {
		return nil, result.err
	}
	token, err := config.Exchange(ctx, result.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("exchange Google authorization code: %w", err)
	}
	return token, nil
}

func authorizationURL(config *oauth2.Config, state, verifier string) string {
	return config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
		oauth2.SetAuthURLParam("prompt", "consent"),
		oauth2.S256ChallengeOption(verifier),
	)
}

func oauthCallbackHandler(state string, resultCh chan<- callbackResult) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "OAuth callback requires GET", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("state") != state {
			http.Error(w, "OAuth state did not match", http.StatusBadRequest)
			return
		}
		if message := r.URL.Query().Get("error"); message != "" {
			http.Error(w, message, http.StatusBadRequest)
			select {
			case resultCh <- callbackResult{err: fmt.Errorf("Google OAuth: %s", message)}:
			default:
			}
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "authorization code missing", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "Authorization complete. You can close this window.")
		select {
		case resultCh <- callbackResult{code: code}:
		default:
		}
	})
}

func randomState() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func openBrowser(target string) error {
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{target}
	case "linux":
		command, args = "xdg-open", []string{target}
	default:
		return fmt.Errorf("automatic browser opening is unsupported on %s", runtime.GOOS)
	}
	return exec.Command(command, args...).Start()
}

func loadToken(path string) (*oauth2.Token, error) {
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var token oauth2.Token
	if err := json.NewDecoder(file).Decode(&token); err != nil {
		return nil, err
	}
	return &token, nil
}

func saveToken(path string, token *oauth2.Token) error {
	if err := xdg.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("create Google token directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".oauth-token-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if err := json.NewEncoder(tmp).Encode(token); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("save Google OAuth token: %w", err)
	}
	return os.Chmod(path, 0600)
}

type savingTokenSource struct {
	mu                sync.Mutex
	source            oauth2.TokenSource
	path              string
	current           *oauth2.Token
	authorize         func() (*oauth2.Token, error)
	replacementSource func(*oauth2.Token) oauth2.TokenSource
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, err := s.source.Token()
	if err != nil {
		if !isInvalidGrant(err) || s.authorize == nil || s.replacementSource == nil {
			return nil, err
		}
		if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("remove stale Google OAuth token: %w", err)
		}
		s.current = nil
		token, err = s.authorize()
		if err != nil {
			return nil, err
		}
		s.source = s.replacementSource(token)
	}
	if s.current == nil || token.AccessToken != s.current.AccessToken || token.RefreshToken != s.current.RefreshToken || !token.Expiry.Equal(s.current.Expiry) {
		if err := saveToken(s.path, token); err != nil {
			return nil, err
		}
		s.current = token
	}
	return token, nil
}

func isInvalidGrant(err error) bool {
	var retrieveErr *oauth2.RetrieveError
	return errors.As(err, &retrieveErr) && retrieveErr.ErrorCode == "invalid_grant"
}
