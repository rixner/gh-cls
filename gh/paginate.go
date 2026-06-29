package gh

import "context"

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
