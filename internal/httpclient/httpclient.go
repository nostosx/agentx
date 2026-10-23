package httpclient

import "net/http"

// AddHeaders appends every value of src to dst without retaining its slices.
// dst must be initialized. The caller synchronizes concurrent header mutations.
func AddHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// ClientOr returns c, or http.DefaultClient when c is nil.
func ClientOr(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return http.DefaultClient
}
