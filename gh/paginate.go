package gh

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// pageSize is the number of items requested per page, GitHub's maximum for the
// list endpoints this tool uses. A page returning fewer than this is the last.
const pageSize = 100

// getPaged fetches every page of a paginated GET endpoint and returns the
// concatenation of all items. pathFor builds the request path for a 1-based page
// number (it must request per_page=pageSize); paging stops once a page returns
// fewer than pageSize items. Centralizing the loop keeps every list endpoint
// correct: a hand-rolled single-page fetch can no longer silently drop results.
func getPaged[T any](ctx context.Context, c *restClient, pathFor func(page int) string) ([]T, error) {
	var out []T
	for page := 1; ; page++ {
		var batch []T
		if _, err := c.do(ctx, "GET", pathFor(page), nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if len(batch) < pageSize {
			break
		}
	}
	return out, nil
}

// maxCursorPages bounds a cursor walk. Nothing this tool reads is remotely this
// long, so reaching it means the cursor is not advancing; erroring beats looping
// forever against the API.
const maxCursorPages = 1000

// nextCursor returns the "after" cursor from a Link header's rel="next", or ""
// when the header names no next page.
func nextCursor(h http.Header) string {
	for _, link := range strings.Split(h.Get("Link"), ",") {
		parts := strings.Split(strings.TrimSpace(link), ";")
		if len(parts) < 2 {
			continue
		}
		isNext := false
		for _, p := range parts[1:] {
			if strings.Contains(strings.ReplaceAll(p, `"`, ""), "rel=next") {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(parts[0]), "<>")
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return u.Query().Get("after")
	}
	return ""
}

// getCursorPaged fetches every page of an endpoint that paginates by cursor
// rather than page number, following the Link header's rel="next".
//
// This exists because the repository activity endpoint accepts a `page`
// parameter and silently ignores it: a page-number loop re-reads page one
// forever and never terminates. Only the cursor advances. base must already
// carry per_page and any filters.
func getCursorPaged[T any](ctx context.Context, c *restClient, base string) ([]T, error) {
	var out []T
	cursor := ""
	for page := 0; ; page++ {
		if page >= maxCursorPages {
			return nil, fmt.Errorf("gave up paginating %s after %d pages; the cursor is not advancing", base, maxCursorPages)
		}
		path := base
		if cursor != "" {
			path += "&after=" + url.QueryEscape(cursor)
		}
		var batch []T
		h, err := c.do(ctx, "GET", path, nil, &batch)
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)

		next := nextCursor(h)
		// A repeated cursor would spin on the same page; an empty batch means
		// there is nothing further even if a link is still advertised.
		if next == "" || next == cursor || len(batch) == 0 {
			return out, nil
		}
		cursor = next
	}
}

// selectPaged scans a paginated GET endpoint page by page and returns the first
// item for which match reports true (with found true), without fetching the
// remaining pages. Like getPaged it stops once a page returns fewer than
// pageSize items. When nothing matches it returns the zero value and found false.
func selectPaged[T any](ctx context.Context, c *restClient, pathFor func(page int) string, match func(T) bool) (T, bool, error) {
	for page := 1; ; page++ {
		var batch []T
		if _, err := c.do(ctx, "GET", pathFor(page), nil, &batch); err != nil {
			var zero T
			return zero, false, err
		}
		for _, item := range batch {
			if match(item) {
				return item, true, nil
			}
		}
		if len(batch) < pageSize {
			break
		}
	}
	var zero T
	return zero, false, nil
}
