package gcal

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestOAuthAuthorizeServesCallbackBeforeOpeningURL(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse token request: %v", err)
		}
		if got := r.Form.Get("code"); got != "authorization-code" {
			t.Errorf("code = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"access-token","refresh_token":"refresh-token","token_type":"Bearer"}`)
	}))
	defer tokenServer.Close()

	config := &oauth2.Config{
		ClientID:     "client-id",
		ClientSecret: "client-secret",
		RedirectURL:  oauthCallbackURL,
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.test/authorize",
			TokenURL: tokenServer.URL,
		},
	}
	var openerErr error
	oauth := OAuth{
		Timeout: time.Second,
		OpenURL: func(target string) error {
			parsed, err := url.Parse(target)
			if err != nil {
				openerErr = err
				return err
			}
			callback := oauthCallbackURL + "?state=" + url.QueryEscape(parsed.Query().Get("state")) + "&code=authorization-code"
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get(callback)
			if err != nil {
				openerErr = err
				return err
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode != http.StatusOK {
				openerErr = fmt.Errorf("callback status = %d", response.StatusCode)
				return openerErr
			}
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	token, err := oauth.authorize(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	if openerErr != nil {
		t.Fatal(openerErr)
	}
	if token.AccessToken != "access-token" || token.RefreshToken != "refresh-token" {
		t.Fatalf("token = %#v", token)
	}

	listener, err := net.Listen("tcp", oauthCallbackAddress)
	if err != nil {
		t.Fatalf("OAuth callback listener was not released: %v", err)
	}
	listener.Close()
}

func TestAuthorizationURLRequestsRefreshTokenAndConsentWithPKCE(t *testing.T) {
	config := &oauth2.Config{
		ClientID:    "client-id",
		RedirectURL: oauthCallbackURL,
		Scopes:      []string{"calendar.readonly"},
		Endpoint: oauth2.Endpoint{
			AuthURL: "https://accounts.example.test/authorize",
		},
	}
	const state = "expected-state"
	const verifier = "expected-verifier"

	parsed, err := url.Parse(authorizationURL(config, state, verifier))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	want := map[string]string{
		"access_type":           "offline",
		"prompt":                "consent",
		"code_challenge":        oauth2.S256ChallengeFromVerifier(verifier),
		"code_challenge_method": "S256",
		"state":                 state,
		"redirect_uri":          "http://localhost:8000/callback",
	}
	for key, wantValue := range want {
		if got := query.Get(key); got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
}

func TestOAuthCallbackConfigurationIsFixed(t *testing.T) {
	if oauthCallbackAddress != "localhost:8000" {
		t.Errorf("oauthCallbackAddress = %q", oauthCallbackAddress)
	}
	if oauthCallbackPath != "/callback" {
		t.Errorf("oauthCallbackPath = %q", oauthCallbackPath)
	}
	if oauthCallbackURL != "http://localhost:8000/callback" {
		t.Errorf("oauthCallbackURL = %q", oauthCallbackURL)
	}
}

func TestTokenPersistenceIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli", "google", "oauth-token.json")
	want := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh", Expiry: time.Now().Round(time.Second)}
	if err := saveToken(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Fatalf("token = %#v", got)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dir.Mode().Perm() != 0700 {
		t.Fatalf("directory mode = %o", dir.Mode().Perm())
	}
}

func TestOAuthCallbackIgnoresMalformedRequestsUntilValidCode(t *testing.T) {
	results := make(chan callbackResult, 1)
	handler := oauthCallbackHandler("expected-state", results)

	requests := []string{
		"/callback?state=wrong&code=wrong",
		"/callback?state=expected-state",
	}
	for _, target := range requests {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s status = %d", target, recorder.Code)
		}
		select {
		case result := <-results:
			t.Fatalf("malformed callback produced result %#v", result)
		default:
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/callback?state=expected-state&code=valid", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("valid callback status = %d", recorder.Code)
	}
	select {
	case result := <-results:
		if result.code != "valid" || result.err != nil {
			t.Fatalf("result = %#v", result)
		}
	default:
		t.Fatal("valid callback produced no result")
	}
}

func TestIsInvalidGrant(t *testing.T) {
	invalidGrant := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	if !isInvalidGrant(invalidGrant) {
		t.Fatal("direct invalid_grant was not recognized")
	}
	if !isInvalidGrant(fmt.Errorf("refresh token: %w", invalidGrant)) {
		t.Fatal("wrapped invalid_grant was not recognized")
	}

	tests := []struct {
		name string
		err  error
	}{
		{name: "other OAuth code", err: &oauth2.RetrieveError{ErrorCode: "access_denied"}},
		{name: "empty OAuth code", err: &oauth2.RetrieveError{Response: &http.Response{Status: "500 Internal Server Error"}}},
		{name: "context", err: context.DeadlineExceeded},
		{name: "network", err: errors.New("network unavailable")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if isInvalidGrant(test.err) {
				t.Fatalf("isInvalidGrant(%v) = true", test.err)
			}
		})
	}
}

func TestSavingTokenSourceReauthorizesAndPersistsToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token.json")
	stale := &oauth2.Token{AccessToken: "stale-access", RefreshToken: "stale-refresh"}
	if err := saveToken(path, stale); err != nil {
		t.Fatal(err)
	}

	revokedCalls := 0
	replacementCalls := 0
	authorizeCalls := 0
	replacement := &oauth2.Token{AccessToken: "replacement-access", RefreshToken: "replacement-refresh"}
	source := &savingTokenSource{
		source: tokenSourceFunc(func() (*oauth2.Token, error) {
			revokedCalls++
			return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
		}),
		path:    path,
		current: stale,
		authorize: func() (*oauth2.Token, error) {
			authorizeCalls++
			return replacement, nil
		},
		replacementSource: func(token *oauth2.Token) oauth2.TokenSource {
			if token != replacement {
				t.Fatalf("replacement source token = %#v", token)
			}
			return tokenSourceFunc(func() (*oauth2.Token, error) {
				replacementCalls++
				return &oauth2.Token{AccessToken: "later-access", RefreshToken: "replacement-refresh"}, nil
			})
		},
	}

	got, err := source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got != replacement {
		t.Fatalf("token = %#v, want %#v", got, replacement)
	}
	persisted, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.AccessToken != replacement.AccessToken || persisted.RefreshToken != replacement.RefreshToken {
		t.Fatalf("persisted token = %#v", persisted)
	}

	got, err = source.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "later-access" {
		t.Fatalf("later token = %#v", got)
	}
	if revokedCalls != 1 || replacementCalls != 1 || authorizeCalls != 1 {
		t.Fatalf("calls: revoked=%d replacement=%d authorize=%d", revokedCalls, replacementCalls, authorizeCalls)
	}
}

func TestSavingTokenSourcePreservesTokenForTransientErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "network", err: errors.New("connection reset")},
		{name: "context", err: context.Canceled},
		{name: "server error", err: &oauth2.RetrieveError{Response: &http.Response{Status: "503 Service Unavailable"}}},
		{name: "other OAuth code", err: &oauth2.RetrieveError{ErrorCode: "temporarily_unavailable"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "oauth-token.json")
			stale := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
			if err := saveToken(path, stale); err != nil {
				t.Fatal(err)
			}
			authorized := false
			source := &savingTokenSource{
				source: tokenSourceFunc(func() (*oauth2.Token, error) { return nil, test.err }),
				path:   path,
				authorize: func() (*oauth2.Token, error) {
					authorized = true
					return nil, errors.New("unexpected authorization")
				},
			}

			_, err := source.Token()
			if err != test.err {
				t.Fatalf("error = %v, want original %v", err, test.err)
			}
			if authorized {
				t.Fatal("authorization was invoked")
			}
			got, err := loadToken(path)
			if err != nil {
				t.Fatal(err)
			}
			if got.RefreshToken != stale.RefreshToken {
				t.Fatalf("saved token = %#v", got)
			}
		})
	}
}

func TestSavingTokenSourceLeavesStaleTokenAbsentWhenAuthorizationFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token.json")
	if err := saveToken(path, &oauth2.Token{RefreshToken: "stale"}); err != nil {
		t.Fatal(err)
	}
	authorizeErr := errors.New("authorization failed")
	source := &savingTokenSource{
		source: tokenSourceFunc(func() (*oauth2.Token, error) {
			return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
		}),
		path: path,
		authorize: func() (*oauth2.Token, error) {
			return nil, authorizeErr
		},
		replacementSource: func(token *oauth2.Token) oauth2.TokenSource {
			return oauth2.StaticTokenSource(token)
		},
	}

	_, err := source.Token()
	if err != authorizeErr {
		t.Fatalf("error = %v, want %v", err, authorizeErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale token still exists: %v", err)
	}
}

func TestSavingTokenSourceRecreatesTokenFileWhenReplacementIsUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token.json")
	stale := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
	if err := saveToken(path, stale); err != nil {
		t.Fatal(err)
	}
	replacement := &oauth2.Token{AccessToken: "access", RefreshToken: "refresh"}
	source := &savingTokenSource{
		source: tokenSourceFunc(func() (*oauth2.Token, error) {
			return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
		}),
		path:    path,
		current: stale,
		authorize: func() (*oauth2.Token, error) {
			return replacement, nil
		},
		replacementSource: func(token *oauth2.Token) oauth2.TokenSource {
			return oauth2.StaticTokenSource(token)
		},
	}

	if _, err := source.Token(); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != replacement.AccessToken || got.RefreshToken != replacement.RefreshToken {
		t.Fatalf("persisted token = %#v", got)
	}
}

func TestSavingTokenSourceReauthorizesOnceForConcurrentCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token.json")
	stale := &oauth2.Token{AccessToken: "stale", RefreshToken: "stale-refresh"}
	if err := saveToken(path, stale); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	start := make(chan struct{})
	authorizeStarted := make(chan struct{})
	finishAuthorize := make(chan struct{})
	revokedCalls := 0
	authorizeCalls := 0
	replacementCalls := 0
	replacement := &oauth2.Token{AccessToken: "replacement", RefreshToken: "replacement-refresh"}
	source := &savingTokenSource{
		source: tokenSourceFunc(func() (*oauth2.Token, error) {
			revokedCalls++
			return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
		}),
		path:    path,
		current: stale,
		authorize: func() (*oauth2.Token, error) {
			authorizeCalls++
			close(authorizeStarted)
			<-finishAuthorize
			return replacement, nil
		},
		replacementSource: func(*oauth2.Token) oauth2.TokenSource {
			return tokenSourceFunc(func() (*oauth2.Token, error) {
				replacementCalls++
				return replacement, nil
			})
		},
	}

	results := make(chan *oauth2.Token, callers)
	errs := make(chan error, callers)
	var ready sync.WaitGroup
	var done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			token, err := source.Token()
			results <- token
			errs <- err
		}()
	}
	ready.Wait()
	close(start)
	<-authorizeStarted
	close(finishAuthorize)
	done.Wait()
	close(results)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for token := range results {
		if token != replacement {
			t.Fatalf("token = %#v, want %#v", token, replacement)
		}
	}
	if revokedCalls != 1 || authorizeCalls != 1 || replacementCalls != callers-1 {
		t.Fatalf("calls: revoked=%d authorize=%d replacement=%d", revokedCalls, authorizeCalls, replacementCalls)
	}
}

func TestSavingTokenSourcePersistsRefreshTokenChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth-token.json")
	expiry := time.Now().Add(time.Hour).Round(time.Second)
	current := &oauth2.Token{AccessToken: "access", RefreshToken: "old-refresh", Expiry: expiry}
	if err := saveToken(path, current); err != nil {
		t.Fatal(err)
	}
	source := &savingTokenSource{
		source: tokenSourceFunc(func() (*oauth2.Token, error) {
			return &oauth2.Token{AccessToken: "access", RefreshToken: "new-refresh", Expiry: expiry}, nil
		}),
		path:    path,
		current: current,
	}

	if _, err := source.Token(); err != nil {
		t.Fatal(err)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token = %q", got.RefreshToken)
	}
}

type tokenSourceFunc func() (*oauth2.Token, error)

func (f tokenSourceFunc) Token() (*oauth2.Token, error) {
	return f()
}
