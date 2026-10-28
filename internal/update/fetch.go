package update

import (
	"context"
	"net/http"
	"net/url"
)

// Fetch GETs ref through the page grant, for what else an app reads off its
// release page — Mega Voice's models. ref is absolute or relative to FeedURL:
// "models.megavoice-mac?raw=1" is that record, "/f/<slug>/<id>" a file. header
// is copied onto the request (Range, to resume a download). The grant rules
// are Check's: one re-unlock on a 401, then ErrRevoked; a build with no
// Channel answers ErrDisabled without a request. The caller closes the body.
func (u *Updater) Fetch(ctx context.Context, ref string, header http.Header) (*http.Response, error) {
	if !u.Enabled() {
		return nil, ErrDisabled
	}
	r, err := url.Parse(ref)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.feed.ResolveReference(r).String(), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header[k] = v
	}
	return u.do(ctx, req)
}
