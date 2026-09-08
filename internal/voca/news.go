package voca

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/net/html"
)

type ArticleLink struct {
	Title   string
	URL     string
	Related []ArticleLink
}

type ArticlePage struct{ Title, Body, URL string }

type APScraper struct {
	HTTP    *http.Client
	BaseURL string
}

var defaultAPHTTPClient = NewAPHTTPClient()

func NewAPHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	// Clear inherited HTTP/2 negotiation as well as restricting request protocols.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}

func (s APScraper) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return defaultAPHTTPClient
}

func (s APScraper) base() string {
	if s.BaseURL != "" {
		return s.BaseURL
	}
	return "https://apnews.com/"
}

func (s APScraper) fetch(ctx context.Context, target string) (*html.Node, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cli/1.0 (+personal terminal reader)")
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("AP News returned %s", resp.Status)
	}
	doc, err := html.Parse(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("parse AP News HTML: %w", err)
	}
	return doc, nil
}

func (s APScraper) Headlines(ctx context.Context) ([]ArticleLink, error) {
	doc, err := s.fetch(ctx, s.base())
	if err != nil {
		return nil, fmt.Errorf("fetch AP News: %w", err)
	}
	base, err := url.Parse(s.base())
	if err != nil {
		return nil, err
	}
	links := parseHeadlines(doc, base)
	if len(links) == 0 {
		return nil, fmt.Errorf("AP News page contained no recognizable headlines")
	}
	return links, nil
}

func parseHeadlines(doc *html.Node, base *url.URL) []ArticleLink {
	container := firstNodeWithClass(doc, "TwoColumnContainer7030-container")
	if container == nil {
		return nil
	}

	var result []ArticleLink
	seen := map[string]bool{}
	topLevel := map[string]int{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if hasClass(n, "PageListStandardE-items") {
			main, related, ok := headlineGroup(n, base)
			if !ok {
				return
			}
			index, exists := topLevel[main.URL]
			if !exists {
				if seen[main.URL] {
					return
				}
				seen[main.URL] = true
				result = append(result, main)
				index = len(result) - 1
				topLevel[main.URL] = index
			}
			for _, link := range related {
				if !seen[link.URL] {
					result[index].Related = append(result[index].Related, link)
					seen[link.URL] = true
				}
			}
			return
		}
		if isHeadline(n) {
			if link, ok := headlineLink(n, base); ok && !seen[link.URL] {
				seen[link.URL] = true
				result = append(result, link)
				topLevel[link.URL] = len(result) - 1
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(container)
	return result
}

func firstNodeWithClass(root *html.Node, class string) *html.Node {
	if hasClass(root, class) {
		return root
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if found := firstNodeWithClass(child, class); found != nil {
			return found
		}
	}
	return nil
}

func headlineGroup(root *html.Node, base *url.URL) (ArticleLink, []ArticleLink, bool) {
	var main ArticleLink
	var related []ArticleLink
	var foundMain bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n != root && hasClass(n, "PageListStandardE-items-secondary") {
			related = append(related, headlineLinks(n, base)...)
			return
		}
		if isHeadline(n) {
			if !foundMain {
				main, foundMain = headlineLink(n, base)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return main, related, foundMain
}

func headlineLinks(root *html.Node, base *url.URL) []ArticleLink {
	var links []ArticleLink
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if isHeadline(n) {
			if link, ok := headlineLink(n, base); ok {
				links = append(links, link)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(root)
	return links
}

func isHeadline(n *html.Node) bool {
	return n.Type == html.ElementNode && n.Data == "bsp-custom-headline"
}

func headlineLink(root *html.Node, base *url.URL) (ArticleLink, bool) {
	if root.Type == html.ElementNode && root.Data == "a" {
		return articleLink(root, base)
	}
	for child := root.FirstChild; child != nil; child = child.NextSibling {
		if link, ok := headlineLink(child, base); ok {
			return link, true
		}
	}
	return ArticleLink{}, false
}

func articleLink(anchor *html.Node, base *url.URL) (ArticleLink, bool) {
	if anchor.Type != html.ElementNode || anchor.Data != "a" {
		return ArticleLink{}, false
	}
	href := attr(anchor, "href")
	title := strings.Join(strings.Fields(nodeText(anchor)), " ")
	if href == "" || len(strings.Fields(title)) < 3 {
		return ArticleLink{}, false
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return ArticleLink{}, false
	}
	resolved := base.ResolveReference(parsed)
	if !strings.EqualFold(resolved.Scheme, "https") || resolved.Host != base.Host || !strings.Contains(resolved.Path, "/article/") {
		return ArticleLink{}, false
	}
	return ArticleLink{Title: title, URL: resolved.String()}, true
}

func (s APScraper) Article(ctx context.Context, target string) (ArticlePage, error) {
	doc, err := s.fetch(ctx, target)
	if err != nil {
		return ArticlePage{}, err
	}
	page := parseArticle(doc)
	page.URL = target
	if page.Title == "" || page.Body == "" {
		return ArticlePage{}, fmt.Errorf("AP News article content was not found")
	}
	return page, nil
}

func parseArticle(doc *html.Node) ArticlePage {
	header := firstNodeWithClass(doc, "Page-headline")
	content := firstNodeWithClass(doc, "RichTextBody")
	if header == nil || content == nil {
		return ArticlePage{}
	}

	var body strings.Builder
	var walk func(*html.Node)
	walk = func(parent *html.Node) {
		for child := parent.FirstChild; child != nil; child = child.NextSibling {
			if child.Type != html.ElementNode {
				continue
			}
			text := nodeText(child)
			if text == "" {
				continue
			}
			switch child.Data {
			case "div":
				if hasClass(child, "Infobox") {
					walk(child)
				} else if hasClass(child, "Infobox-items") && child.FirstChild != nil {
					body.WriteString("## Information\n\n")
					walk(child)
				}
			case "ul":
				walk(child)
			case "li":
				body.WriteString("- " + text + "\n\n")
			case "p":
				if nodeUnderClass(child, "Infobox") {
					body.WriteString("### " + text + "\n\n")
				} else {
					body.WriteString(text + "\n\n")
				}
			case "h2", "h3", "h4", "h5", "h6":
				level := strings.TrimPrefix(child.Data, "h")
				count, _ := strconv.Atoi(level)
				body.WriteString(strings.Repeat("#", count) + " " + text + "\n\n")
			}
		}
	}
	walk(content)
	return ArticlePage{Title: nodeText(header), Body: body.String()}
}

func nodeUnderClass(n *html.Node, class string) bool {
	for parent := n.Parent; parent != nil; parent = parent.Parent {
		if hasClass(parent, class) {
			return true
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.TextNode {
			text.WriteString(node.Data)
			return
		}
		if node.Type == html.ElementNode && node.Data == "br" {
			text.WriteByte(' ')
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(text.String()), " ")
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func hasClass(n *html.Node, class string) bool {
	for _, value := range strings.Fields(attr(n, "class")) {
		if value == class {
			return true
		}
	}
	return false
}

type Pager interface {
	Show(context.Context, ArticlePage, io.Reader, io.Writer, io.Writer) error
}
type TerminalPager struct{}

const osc8Marker = "\x1b]8;;"

const headingColorAWK = `{ if ($0 ~ /^###### /) print "\033[1;35m" $0 "\033[0m"; else if ($0 ~ /^##### /) print "\033[1;36m" $0 "\033[0m"; else if ($0 ~ /^#### /) print "\033[1;34m" $0 "\033[0m"; else if ($0 ~ /^### /) print "\033[1;32m" $0 "\033[0m"; else if ($0 ~ /^## /) print "\033[1;33m" $0 "\033[0m"; else if ($0 ~ /^# /) print "\033[1;35m" $0 "\033[0m"; else print }`

func articleContent(page ArticlePage) string {
	return fmt.Sprintf("\n# \x1b]8;;%s\a%s\x1b]8;;\a\n\n%s", page.URL, page.Title, page.Body)
}

func headingColor(line string) string {
	colors := []struct {
		prefix string
		code   string
	}{
		{"###### ", "1;35"},
		{"##### ", "1;36"},
		{"#### ", "1;34"},
		{"### ", "1;32"},
		{"## ", "1;33"},
		{"# ", "1;35"},
	}
	for _, color := range colors {
		if strings.HasPrefix(line, color.prefix) {
			return color.code
		}
	}
	return ""
}

type commandOutput func(context.Context, string, ...string) ([]byte, error)

func terminalWidth(ctx context.Context, tmux string, lookPath func(string) (string, error), output commandOutput) int {
	if tmux != "" {
		if width, ok := commandWidth(ctx, "tmux", []string{"display-message", "-p", "#{pane_width}"}, lookPath, output); ok {
			return width
		}
	}
	width, _ := commandWidth(ctx, "tput", []string{"cols"}, lookPath, output)
	return width
}

func commandWidth(ctx context.Context, name string, args []string, lookPath func(string) (string, error), output commandOutput) (int, bool) {
	path, err := lookPath(name)
	if err != nil {
		return 0, false
	}
	value, err := output(ctx, path, args...)
	if err != nil {
		return 0, false
	}
	width, err := strconv.Atoi(strings.TrimSpace(string(value)))
	if err != nil || width <= 0 {
		return 0, false
	}
	if width > 100 {
		width = 100
	}
	return width, true
}

type foldLineFunc func(context.Context, string, int) (string, error)

func foldArticle(ctx context.Context, content string, width int, foldLine foldLineFunc) (string, error) {
	if width <= 0 {
		return content, nil
	}
	var result strings.Builder
	for content != "" {
		line := content
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			line = content[:newline+1]
			content = content[newline+1:]
		} else {
			content = ""
		}
		if strings.Contains(line, osc8Marker) {
			result.WriteString(line)
			continue
		}
		folded, err := foldLine(ctx, line, width)
		if err != nil {
			return "", err
		}
		result.WriteString(folded)
	}
	return result.String(), nil
}

func runFold(ctx context.Context, line string, width int) (string, error) {
	cmd := exec.CommandContext(ctx, "fold", "-s", "-w", strconv.Itoa(width))
	cmd.Stdin = strings.NewReader(line)
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wrap article: %w", err)
	}
	return string(output), nil
}

func execOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

func requireCommands(names []string, lookPath func(string) (string, error)) error {
	for _, name := range names {
		if _, err := lookPath(name); err != nil {
			return fmt.Errorf("news pager requires %q in PATH", name)
		}
	}
	return nil
}

func requireFold(width int, lookPath func(string) (string, error)) error {
	if width <= 0 {
		return nil
	}
	return requireCommands([]string{"fold"}, lookPath)
}

func formatterPipeClosed(err error) bool {
	if errors.Is(err, syscall.EPIPE) {
		return true
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGPIPE
}

func (TerminalPager) Show(ctx context.Context, page ArticlePage, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := requireCommands([]string{"awk", "less"}, exec.LookPath); err != nil {
		return err
	}
	width := terminalWidth(ctx, os.Getenv("TMUX"), exec.LookPath, execOutput)
	if err := requireFold(width, exec.LookPath); err != nil {
		return err
	}
	content, err := foldArticle(ctx, articleContent(page), width, runFold)
	if err != nil {
		return err
	}

	awk := exec.CommandContext(ctx, "awk", headingColorAWK)
	less := exec.CommandContext(ctx, "less", "-Rc")
	awk.Stdin = strings.NewReader(content)
	reader, writer, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("create article pipe: %w", err)
	}
	awk.Stdout = writer
	less.Stdin, less.Stdout, less.Stderr = reader, stdout, stderr
	if err := less.Start(); err != nil {
		reader.Close()
		writer.Close()
		return fmt.Errorf("page article: %w", err)
	}
	reader.Close()
	if err := awk.Start(); err != nil {
		writer.Close()
		_ = less.Process.Kill()
		_ = less.Wait()
		return fmt.Errorf("format article: %w", err)
	}
	writer.Close()
	lessErr := less.Wait()
	awkErr := awk.Wait()
	if awkErr != nil && !formatterPipeClosed(awkErr) {
		return fmt.Errorf("format article: %w", awkErr)
	}
	if lessErr != nil {
		return fmt.Errorf("page article: %w", lessErr)
	}
	_ = stdin
	return nil
}

type News struct {
	Scraper APScraper
	Pager   Pager
}

func (n News) Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) error {
	articles, err := n.Scraper.Headlines(ctx)
	if err != nil {
		return err
	}
	pager := n.Pager
	if pager == nil {
		pager = TerminalPager{}
	}
	reader := bufio.NewReader(stdin)
	for {
		if err := renderHeadlineTable(stdout, articles); err != nil {
			return err
		}
		if _, err := fmt.Fprint(stdout, "Select an article (q to quit): "); err != nil {
			return err
		}
		choice, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		choice = strings.TrimSpace(choice)
		if choice == "q" || (err == io.EOF && choice == "") {
			return nil
		}
		link, selectErr := selectArticle(articles, choice)
		if selectErr != nil {
			fmt.Fprintln(stderr, selectErr)
			if err == io.EOF {
				return nil
			}
			continue
		}
		page, fetchErr := n.Scraper.Article(ctx, link.URL)
		if fetchErr != nil {
			fmt.Fprintln(stderr, fetchErr)
			if err == io.EOF {
				return nil
			}
			continue
		}
		if pageErr := pager.Show(ctx, page, stdin, stdout, stderr); pageErr != nil {
			return pageErr
		}
		if err == io.EOF {
			return nil
		}
	}
}

func renderHeadlineTable(output io.Writer, articles []ArticleLink) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "Index\tTitle")
	for i, article := range articles {
		fmt.Fprintf(writer, "%d\t%s\n", i+1, article.Title)
		for j, related := range article.Related {
			fmt.Fprintf(writer, "  %d-%d\t%s\n", i+1, j+1, related.Title)
		}
	}
	return writer.Flush()
}

func selectArticle(articles []ArticleLink, choice string) (ArticleLink, error) {
	parts := strings.Split(choice, "-")
	if len(parts) < 1 || len(parts) > 2 {
		return ArticleLink{}, fmt.Errorf("invalid article index %q", choice)
	}
	main, err := strconv.Atoi(parts[0])
	if err != nil || main < 1 || main > len(articles) {
		return ArticleLink{}, fmt.Errorf("invalid article index %q", choice)
	}
	if len(parts) == 1 {
		return articles[main-1], nil
	}
	related, err := strconv.Atoi(parts[1])
	if err != nil || related < 1 || related > len(articles[main-1].Related) {
		return ArticleLink{}, fmt.Errorf("invalid article index %q", choice)
	}
	return articles[main-1].Related[related-1], nil
}
