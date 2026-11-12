package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/metabbe3/GoAgent/internal/guardrail"
	"github.com/metabbe3/GoAgent/internal/llm"
)

const (
	webTimeout       = 15 * time.Second
	webFetchMaxBytes = 256 << 10 // 256 KiB
	webResultMaxCap  = 8000
)

var (
	cgnatRange  = netip.MustParsePrefix("100.64.0.0/10")
	benchRange  = netip.MustParsePrefix("198.18.0.0/15")
	thisNetRng  = netip.MustParsePrefix("0.0.0.0/8")
	nat64Prefix = netip.MustParsePrefix("64:ff9b::/96")
)

func isBlockedIP(a netip.Addr) bool {
	a = a.Unmap()
	return !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsMulticast() || a.IsUnspecified() ||
		cgnatRange.Contains(a) || benchRange.Contains(a) || thisNetRng.Contains(a) ||
		(a.Is6() && nat64Prefix.Contains(a))
}

func guardedTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, addr string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return fmt.Errorf("bad dial address %q", addr)
			}
			ip, err := netip.ParseAddr(host)
			if err != nil || isBlockedIP(ip) {
				return fmt.Errorf("address %q is not a public IP (loopback and private networks blocked)", host)
			}
			return nil
		},
	}
	return &http.Transport{DialContext: dialer.DialContext}
}

var (
	scriptBlockRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blockElRe     = regexp.MustCompile(`(?is)</(p|div|br|li|tr|h[1-6])>`)
	anyTagRe      = regexp.MustCompile(`<[^>]*>`)
)

func (r *Registry) registerWebTools() {
	r.Register(llm.ToolDefinition{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "fetch_url",
			Description: "Fetch content from a public http(s) URL and return its readable text. " +
				"Private, localhost, and loopback IP addresses are blocked by guardrails.",
			Parameters: schema(
				map[string]any{
					"url": map[string]any{"type": "string", "description": "Absolute public http(s) URL to fetch"},
				},
				[]string{"url"},
			),
		},
	}, r.handleFetchURL)
}

func (r *Registry) handleFetchURL(ctx context.Context, argsJSON string) (string, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	raw := strings.TrimSpace(args.URL)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("url must be an absolute http or https URL, got %q", raw)
	}

	client := &http.Client{
		Transport: guardedTransport(),
		Timeout:   webTimeout,
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "GoAgent/0.3 (Universal Agent; GoMesh)")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch failed (SSRF guard or network error): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("fetch failed: HTTP status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, webFetchMaxBytes))
	if err != nil {
		return "", fmt.Errorf("failed to read response: %w", err)
	}

	text := htmlToText(string(body))
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("fetched page contains no readable text")
	}

	if len(text) > webResultMaxCap {
		text = text[:webResultMaxCap] + "\n...[truncated]"
	}

	framed := guardrail.FrameDocumentContent("url "+u.Host, text)
	return framed, nil
}

func htmlToText(s string) string {
	s = scriptBlockRe.ReplaceAllString(s, "")
	s = blockElRe.ReplaceAllString(s, "\n")
	s = anyTagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	var sb strings.Builder
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if sb.Len() > 0 {
				sb.WriteByte('\n')
			}
			sb.WriteString(line)
		}
	}
	return sb.String()
}
