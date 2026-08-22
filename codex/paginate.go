package codex

import (
	"context"
	"iter"

	"github.com/ccheers/codexadkv2/codex/protocol"
)

// maxPages bounds a pagination loop by default, so a server that keeps handing
// back a non-nil cursor cannot spin forever.
const maxPages = 1000

// ListThreads iterates every stored thread matching params, fetching pages as
// needed.
//
// A nil NextCursor from the server means the last page. The iterator stops on the
// first error, yielding it once:
//
//	for thread, err := range client.ListThreads(ctx, params) {
//	    if err != nil {
//	        return err
//	    }
//	    fmt.Println(thread.ID, thread.Preview)
//	}
//
// Breaking out of the loop early is safe and stops fetching. Params.Cursor is
// used as the starting point, and the caller's copy is not modified.
func (c *Client) ListThreads(ctx context.Context, params protocol.ThreadListParams) iter.Seq2[*protocol.Thread, error] {
	return func(yield func(*protocol.Thread, error) bool) {
		page := params
		for pages := 0; pages < maxPages; pages++ {
			out, err := c.ThreadList(ctx, page)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, thread := range out.Data {
				if !yield(thread, nil) {
					return
				}
			}
			// A nil cursor means the final page. An empty-but-present cursor is
			// treated the same way, since it cannot address anything.
			if out.NextCursor == nil || *out.NextCursor == "" {
				return
			}
			// An empty page with a cursor would otherwise loop without progress.
			if len(out.Data) == 0 {
				return
			}
			cursor := *out.NextCursor
			page.Cursor = &cursor
		}
		yield(nil, errTooManyPages)
	}
}

// ListModels iterates every available model, fetching pages as needed.
//
// By default the server returns only picker-visible models; set
// params.IncludeHidden for the full list.
func (c *Client) ListModels(ctx context.Context, params protocol.ModelListParams) iter.Seq2[*protocol.Model, error] {
	return func(yield func(*protocol.Model, error) bool) {
		page := params
		for pages := 0; pages < maxPages; pages++ {
			out, err := c.ModelList(ctx, page)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, model := range out.Data {
				if !yield(model, nil) {
					return
				}
			}
			if out.NextCursor == nil || *out.NextCursor == "" || len(out.Data) == 0 {
				return
			}
			cursor := *out.NextCursor
			page.Cursor = &cursor
		}
		yield(nil, errTooManyPages)
	}
}

// errTooManyPages guards against a server that never stops paginating.
var errTooManyPages = errPagination("codex: stopped after " + itoa(maxPages) +
	" pages; the server kept returning a next cursor")

type errPagination string

func (e errPagination) Error() string { return string(e) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
