package voca

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"
)

func TestAPHTTPClientUsesHTTP1(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Request-Protocol", r.Proto)
		w.WriteHeader(http.StatusOK)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name   string
		client *http.Client
		want   string
	}{
		{"constructed", NewAPHTTPClient(), "HTTP/1.1"},
		{"nil scraper client", (APScraper{}).client(), "HTTP/1.1"},
		{"unchanged default transport", &http.Client{Transport: http.DefaultTransport, Timeout: 15 * time.Second}, "HTTP/2.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.client.Timeout != 15*time.Second {
				t.Fatalf("timeout = %v, want 15s", tc.client.Timeout)
			}
			original := tc.client.Transport.(*http.Transport)
			if original.Proxy == nil || original.DialContext == nil || original.TLSHandshakeTimeout != 10*time.Second {
				t.Fatal("standard proxy, dial, or TLS timeout defaults were lost")
			}
			transport := original.Clone()
			transport.TLSClientConfig.RootCAs = server.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
			if transport.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("TLS certificate verification is disabled")
			}
			t.Cleanup(transport.CloseIdleConnections)
			client := *tc.client
			client.Transport = transport
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.Proto != tc.want || resp.Header.Get("Request-Protocol") != tc.want {
				t.Fatalf("response protocol = %s, request protocol = %s, want %s", resp.Proto, resp.Header.Get("Request-Protocol"), tc.want)
			}
		})
	}
	custom := &http.Client{Timeout: time.Second}
	if (APScraper{HTTP: custom}).client() != custom {
		t.Fatal("injected client was replaced")
	}
}

func TestAPScraperRejectsForbidden(t *testing.T) {
	for _, command := range []string{"headlines", "article"} {
		t.Run(command, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if got := r.Header.Get("User-Agent"); got != "cli/1.0 (+personal terminal reader)" {
					t.Errorf("User-Agent = %q", got)
				}
				w.Header().Set("Cf-Mitigated", "challenge")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<h1 class="Page-headline">Challenge</h1><div class="RichTextBody"><p>Not article content.</p></div>`)
			}))
			t.Cleanup(server.Close)
			client := NewAPHTTPClient()
			t.Cleanup(client.CloseIdleConnections)
			scraper := APScraper{HTTP: client, BaseURL: server.URL}
			var err error
			want := "AP News returned 403 Forbidden"
			if command == "headlines" {
				_, err = scraper.Headlines(context.Background())
				want = "fetch AP News: " + want
			} else {
				_, err = scraper.Article(context.Background(), server.URL)
			}
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
		})
	}
}

func TestParseHeadlinesPreservesAPHierarchyAndDocumentOrder(t *testing.T) {
	file, err := os.Open("testdata/ap_headlines.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	doc, err := html.Parse(file)
	if err != nil {
		t.Fatal(err)
	}

	base, _ := url.Parse("https://apnews.com/")
	got := parseHeadlines(doc, base)
	want := []ArticleLink{
		{Title: "Standalone headline before groups", URL: "https://apnews.com/article/standalone-before"},
		{
			Title: "First main headline",
			URL:   "https://apnews.com/article/main-one",
			Related: []ArticleLink{
				{Title: "First related headline here", URL: "https://apnews.com/article/related-one-a"},
				{Title: "Second related headline here", URL: "https://apnews.com/article/related-one-b"},
				{Title: "Never promoted secondary headline", URL: "https://apnews.com/article/never-promoted"},
			},
		},
		{
			Title:   "Second main headline here",
			URL:     "https://apnews.com/article/main-two",
			Related: []ArticleLink{{Title: "Group two related headline", URL: "https://apnews.com/article/related-two"}},
		},
		{Title: "Standalone headline after groups", URL: "https://apnews.com/article/standalone-after"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeadlines() = %#v, want %#v", got, want)
	}
}

func TestParseHeadlinesRequiresRecognizedHierarchy(t *testing.T) {
	fixture := `<html><body>
		<div class="PagePromo"><bsp-custom-headline><a href="/article/promo">Promo headline has words</a></bsp-custom-headline></div>
		<a href="/article/fallback">Fallback headline has words</a>
	</body></html>`
	doc, err := html.Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://apnews.com/")
	if got := parseHeadlines(doc, base); len(got) != 0 {
		t.Fatalf("parseHeadlines() flattened unrecognized markup: %#v", got)
	}
}

func TestParseHeadlinesDoesNotPromoteRelatedWithoutMain(t *testing.T) {
	fixture := `<div class="TwoColumnContainer7030-container">
		<div class="PageListStandardE-items">
			<bsp-custom-headline><a href="https://example.com/article/invalid-main">Invalid main headline here</a></bsp-custom-headline>
			<div class="PageListStandardE-items-secondary">
				<bsp-custom-headline><a href="/article/related">Valid related headline here</a></bsp-custom-headline>
			</div>
		</div>
	</div>`
	doc, err := html.Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://apnews.com/")
	if got := parseHeadlines(doc, base); len(got) != 0 {
		t.Fatalf("parseHeadlines() promoted a related story: %#v", got)
	}
}

func TestParseHeadlinesDoesNotMergeGroupWhoseMainWasOnlyRelated(t *testing.T) {
	fixture := `<div class="TwoColumnContainer7030-container">
		<div class="PageListStandardE-items">
			<bsp-custom-headline><a href="/article/first-main">First main headline here</a></bsp-custom-headline>
			<div class="PageListStandardE-items-secondary">
				<bsp-custom-headline><a href="/article/later-main">Later main first appears related</a></bsp-custom-headline>
			</div>
		</div>
		<div class="PageListStandardE-items">
			<bsp-custom-headline><a href="/article/later-main">Later duplicate main headline</a></bsp-custom-headline>
			<div class="PageListStandardE-items-secondary">
				<bsp-custom-headline><a href="/article/unsafe-child">Unsafe child headline here</a></bsp-custom-headline>
			</div>
		</div>
	</div>`
	doc, err := html.Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://apnews.com/")
	got := parseHeadlines(doc, base)
	want := []ArticleLink{{
		Title: "First main headline here",
		URL:   "https://apnews.com/article/first-main",
		Related: []ArticleLink{{
			Title: "Later main first appears related",
			URL:   "https://apnews.com/article/later-main",
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeadlines() = %#v, want %#v", got, want)
	}
}

func TestParseHeadlinesRequiresHTTPS(t *testing.T) {
	fixture := `<div class="TwoColumnContainer7030-container">
		<bsp-custom-headline><a href="/article/relative">Relative headline remains valid</a></bsp-custom-headline>
		<bsp-custom-headline><a href="https://apnews.com/article/secure">Explicit HTTPS headline remains valid</a></bsp-custom-headline>
		<bsp-custom-headline><a href="http://apnews.com/article/insecure">Explicit HTTP headline is rejected</a></bsp-custom-headline>
		<bsp-custom-headline><a href="ftp://apnews.com/article/ftp">Explicit FTP headline is rejected</a></bsp-custom-headline>
	</div>`
	doc, err := html.Parse(strings.NewReader(fixture))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := url.Parse("https://apnews.com/")
	got := parseHeadlines(doc, base)
	want := []ArticleLink{
		{Title: "Relative headline remains valid", URL: "https://apnews.com/article/relative"},
		{Title: "Explicit HTTPS headline remains valid", URL: "https://apnews.com/article/secure"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseHeadlines() = %#v, want %#v", got, want)
	}
}

func TestRenderHeadlineTableIndentsRelatedIndexes(t *testing.T) {
	articles := []ArticleLink{{
		Title: "Main headline",
		Related: []ArticleLink{
			{Title: "First related"},
			{Title: "Second related"},
		},
	}, {Title: "Another main"}}
	var output bytes.Buffer
	if err := renderHeadlineTable(&output, articles); err != nil {
		t.Fatal(err)
	}
	want := "Index  Title\n" +
		"1      Main headline\n" +
		"  1-1  First related\n" +
		"  1-2  Second related\n" +
		"2      Another main\n"
	if output.String() != want {
		t.Fatalf("renderHeadlineTable() = %q, want %q", output.String(), want)
	}
}

func TestSelectArticle(t *testing.T) {
	articles := []ArticleLink{{
		Title:   "Main",
		Related: []ArticleLink{{Title: "Related"}},
	}, {Title: "Second"}}
	tests := []struct {
		name    string
		choice  string
		want    string
		wantErr bool
	}{
		{name: "main", choice: "2", want: "Second"},
		{name: "related", choice: "1-1", want: "Related"},
		{name: "main out of range", choice: "3", wantErr: true},
		{name: "related out of range", choice: "1-2", wantErr: true},
		{name: "invalid format", choice: "1-1-1", wantErr: true},
		{name: "not a number", choice: "one", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectArticle(articles, tt.choice)
			if (err != nil) != tt.wantErr {
				t.Fatalf("selectArticle() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got.Title != tt.want {
				t.Fatalf("selectArticle() title = %q, want %q", got.Title, tt.want)
			}
		})
	}
}

func TestParseArticle(t *testing.T) {
	file, err := os.Open("testdata/ap_article.html")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	doc, err := html.Parse(file)
	if err != nil {
		t.Fatal(err)
	}
	got := parseArticle(doc)
	want := ArticlePage{
		Title: "Selected AP headline",
		Body: "Opening paragraph.\n\n" +
			"## Information\n\n" +
			"### First fact\n\n" +
			"### Second fact with nested text\n\n" +
			"- First list item\n\n" +
			"- Second list item\n\n" +
			"## Level two\n\n" +
			"### Level three\n\n" +
			"#### Level four\n\n" +
			"##### Level five\n\n" +
			"###### Level six\n\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseArticle() = %#v, want %#v", got, want)
	}
}

func TestNodeTextPreservesInlineBoundaries(t *testing.T) {
	tests := []struct {
		name string
		html string
		want string
	}{
		{name: "inline punctuation", html: `<p>Read <a>this</a>.</p>`, want: "Read this."},
		{name: "adjacent inline nodes without whitespace", html: `<p><span>one</span><span>two</span></p>`, want: "onetwo"},
		{name: "mixed whitespace", html: "<p>  Read\n\t<strong>this</strong>   now. </p>", want: "Read this now."},
		{name: "br separation", html: `<p>first<br>second</p>`, want: "first second"},
		{name: "whitespace only", html: "<p> \n\t </p>", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc, err := html.Parse(strings.NewReader(tt.html))
			if err != nil {
				t.Fatal(err)
			}
			if got := nodeText(doc); got != tt.want {
				t.Fatalf("nodeText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseArticleRequiresPageHeadlineAndRichTextBody(t *testing.T) {
	for _, fixture := range []string{
		`<h1>Wrong heading</h1><div class="RichTextBody"><p>Body</p></div>`,
		`<h1 class="Page-headline">Title</h1><div data-key="article"><p>Fallback</p></div>`,
	} {
		doc, err := html.Parse(strings.NewReader(fixture))
		if err != nil {
			t.Fatal(err)
		}
		if got := parseArticle(doc); got.Title != "" || got.Body != "" {
			t.Fatalf("parseArticle() = %#v, want empty page", got)
		}
	}
}

func TestArticleReturnsContentNotFoundWithoutPageHeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `<h1>Wrong heading</h1><div class="RichTextBody"><p>Body</p></div>`)
	}))
	defer server.Close()

	_, err := (APScraper{HTTP: server.Client()}).Article(context.Background(), server.URL)
	if err == nil || err.Error() != "AP News article content was not found" {
		t.Fatalf("Article() error = %v, want content-not-found error", err)
	}
}

func TestArticleContentExactBytes(t *testing.T) {
	page := ArticlePage{Title: "Linked title", URL: "https://apnews.com/article/example", Body: "Body.\n\n"}
	want := "\n# \x1b]8;;https://apnews.com/article/example\aLinked title\x1b]8;;\a\n\nBody.\n\n"
	if got := articleContent(page); got != want {
		t.Fatalf("articleContent() = %q, want %q", got, want)
	}
}

func TestHeadingColors(t *testing.T) {
	tests := map[string]string{
		"# one":         "1;35",
		"## two":        "1;33",
		"### three":     "1;32",
		"#### four":     "1;34",
		"##### five":    "1;36",
		"###### six":    "1;35",
		"ordinary":      "",
		"####### seven": "",
	}
	for line, want := range tests {
		if got := headingColor(line); got != want {
			t.Errorf("headingColor(%q) = %q, want %q", line, got, want)
		}
	}
}

func TestTerminalWidth(t *testing.T) {
	failure := errors.New("failed")
	tests := []struct {
		name          string
		tmux          string
		tmuxLookupErr error
		tmuxOutput    string
		tmuxOutputErr error
		tputLookupErr error
		tputOutput    string
		tputOutputErr error
		want          int
		wantAttempts  []string
	}{
		{
			name: "without tmux uses tput", tputOutput: "72\n", want: 72,
			wantAttempts: []string{"lookup:tput", "run:tput"},
		},
		{
			name: "tmux success avoids tput and caps width", tmux: "/tmp/tmux", tmuxOutput: "160\n", want: 100,
			wantAttempts: []string{"lookup:tmux", "run:tmux"},
		},
		{
			name: "tmux lookup failure falls back", tmux: "/tmp/tmux", tmuxLookupErr: failure, tputOutput: "81\n", want: 81,
			wantAttempts: []string{"lookup:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "tmux execution failure falls back", tmux: "/tmp/tmux", tmuxOutputErr: failure, tputOutput: "82\n", want: 82,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "empty tmux result falls back", tmux: "/tmp/tmux", tmuxOutput: " \n", tputOutput: "83\n", want: 83,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "invalid tmux result falls back", tmux: "/tmp/tmux", tmuxOutput: "unknown\n", tputOutput: "84\n", want: 84,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "zero tmux result falls back", tmux: "/tmp/tmux", tmuxOutput: "0\n", tputOutput: "85\n", want: 85,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "negative tmux result falls back", tmux: "/tmp/tmux", tmuxOutput: "-1\n", tputOutput: "86\n", want: 86,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput", "run:tput"},
		},
		{
			name: "both commands fail", tmux: "/tmp/tmux", tmuxOutputErr: failure, tputLookupErr: failure, want: 0,
			wantAttempts: []string{"lookup:tmux", "run:tmux", "lookup:tput"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts []string
			lookup := func(name string) (string, error) {
				attempts = append(attempts, "lookup:"+name)
				if name == "tmux" && tt.tmuxLookupErr != nil {
					return "", tt.tmuxLookupErr
				}
				if name == "tput" && tt.tputLookupErr != nil {
					return "", tt.tputLookupErr
				}
				return "/bin/" + name, nil
			}
			output := func(_ context.Context, path string, _ ...string) ([]byte, error) {
				name := strings.TrimPrefix(path, "/bin/")
				attempts = append(attempts, "run:"+name)
				if name == "tmux" {
					return []byte(tt.tmuxOutput), tt.tmuxOutputErr
				}
				return []byte(tt.tputOutput), tt.tputOutputErr
			}
			got := terminalWidth(context.Background(), tt.tmux, lookup, output)
			if got != tt.want {
				t.Fatalf("terminalWidth() = %d, want %d", got, tt.want)
			}
			if !reflect.DeepEqual(attempts, tt.wantAttempts) {
				t.Fatalf("terminalWidth() attempts = %#v, want %#v", attempts, tt.wantAttempts)
			}
		})
	}
}

func TestRequireFoldOnlyWithPositiveWidth(t *testing.T) {
	var lookedUp []string
	missingFold := func(name string) (string, error) {
		lookedUp = append(lookedUp, name)
		return "", errors.New("missing")
	}
	if err := requireFold(0, missingFold); err != nil {
		t.Fatalf("requireFold(0) error = %v", err)
	}
	if len(lookedUp) != 0 {
		t.Fatalf("requireFold(0) looked up %#v, want no lookup", lookedUp)
	}
	if err := requireFold(80, missingFold); err == nil {
		t.Fatal("requireFold(80) returned nil with missing fold")
	}
	if !reflect.DeepEqual(lookedUp, []string{"fold"}) {
		t.Fatalf("requireFold(80) looked up %#v, want fold", lookedUp)
	}
}

func TestFoldArticleSkipsOSCLinksAndFoldsOrdinaryLines(t *testing.T) {
	linked := "# " + osc8Marker + "https://example.com\aA title much longer than the width\x1b]8;;\a\n"
	content := linked + "ordinary body text\n"
	var foldedLines []string
	folder := func(_ context.Context, line string, width int) (string, error) {
		foldedLines = append(foldedLines, line)
		if width != 10 {
			t.Fatalf("fold width = %d, want 10", width)
		}
		return "ordinary\nbody text\n", nil
	}
	got, err := foldArticle(context.Background(), content, 10, folder)
	if err != nil {
		t.Fatal(err)
	}
	want := linked + "ordinary\nbody text\n"
	if got != want {
		t.Fatalf("foldArticle() = %q, want %q", got, want)
	}
	if !reflect.DeepEqual(foldedLines, []string{"ordinary body text\n"}) {
		t.Fatalf("folded lines = %#v", foldedLines)
	}
}
