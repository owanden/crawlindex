package crawler

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

type Policy struct {
	Origins  map[string]bool
	MaxDepth int
}

func (p Policy) Normalize(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || len(raw) > 2048 {
		return "", "", errors.New("invalid URL")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	if (u.Scheme == "http" && u.Port() == "80") || (u.Scheme == "https" && u.Port() == "443") {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.Path == "" {
		u.Path = "/"
	}
	origin := u.Scheme + "://" + u.Host
	if !p.Origins[origin] {
		return "", "", errors.New("origin not allowed")
	}
	return u.String(), origin, nil
}

type Fetcher struct{ Client *http.Client }

const UserAgent = "CrawlIndex/0.1 (controlled-origin crawler)"

func NewFetcher(timeout time.Duration) Fetcher {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return Fetcher{&http.Client{Timeout: timeout, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (f Fetcher) Fetch(ctx context.Context, raw string) (Document, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Document{}, nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := f.Client.Do(req)
	if err != nil {
		return Document{}, nil, errors.New("fetch request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Document{}, nil, fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/html" {
		return Document{}, nil, errors.New("expected text/html")
	}
	const limit = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return Document{}, nil, errors.New("body read failed")
	}
	if len(body) > limit {
		return Document{}, nil, errors.New("body exceeds 1 MiB")
	}
	return Extract(raw, strings.NewReader(string(body)))
}

func Extract(raw string, r io.Reader) (Document, []string, error) {
	root, err := html.Parse(r)
	if err != nil {
		return Document{}, nil, err
	}
	base, err := url.Parse(raw)
	if err != nil {
		return Document{}, nil, err
	}
	var title, body []string
	links := []string{}
	seen := map[string]bool{}
	var walk func(*html.Node, bool)
	walk = func(n *html.Node, inTitle bool) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "noscript", "template":
				return
			}
			if n.Data == "title" {
				inTitle = true
			}
			if n.Data == "a" && len(links) < 32 {
				for _, a := range n.Attr {
					if a.Key == "href" {
						u, e := url.Parse(a.Val)
						if e == nil {
							v := base.ResolveReference(u).String()
							if !seen[v] {
								seen[v] = true
								links = append(links, v)
							}
						}
					}
				}
			}
		}
		if n.Type == html.TextNode {
			if inTitle {
				title = append(title, n.Data)
			} else {
				body = append(body, n.Data)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c, inTitle)
		}
	}
	walk(root, false)
	clean := func(parts []string) string { return strings.Join(strings.Fields(strings.Join(parts, " ")), " ") }
	return Document{URL: raw, Title: clean(title), Body: clean(body)}, links, nil
}
