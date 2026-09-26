Fix pagination inconsistency when ListOptions.PerPage is zero.

ROOT CAUSE
The client sends `?per_page=0` when ListOptions.PerPage holds its Go zero-value (0).
GitHub's API treats `per_page=0` inconsistently — some endpoints return an empty
page, others fall back to the default (30), and the resulting pagination is
non-deterministic. The correct behavior is to OMIT the `per_page` query parameter
entirely when PerPage == 0, letting the API apply its documented default.

SOLUTION — github/github.go

1. Add a ListOptions type and an addOptions URL-encoding helper that only sets
   query parameters for non-zero fields:

```go
// ListOptions specifies the optional parameters to various List methods that
// support pagination.
type ListOptions struct {
    // For paginated result sets, page of results to retrieve.
    Page int `url:"page,omitempty"`

    // For paginated result sets, the number of results to include per page.
    PerPage int `url:"per_page,omitempty"`
}

// addOptions adds the parameters in opts as URL query parameters to s.
// opts must be a struct whose fields may contain "url" tags.
func addOptions(s string, opts interface{}) (string, error) {
    v := reflect.ValueOf(opts)
    if v.Kind() == reflect.Ptr && v.IsNil() {
        return s, nil
    }

    u, err := url.Parse(s)
    if err != nil {
        return s, err
    }

    qs, err := query.Values(opts)
    if err != nil {
        return s, err
    }

    u.RawQuery = qs.Encode()
    return u.String(), nil
}
```

2. Update NewRequest to accept opts and apply addOptions before building the
   request, so that a zero PerPage produces NO `per_page` key (thanks to
   `omitempty` on the struct tag):

```go
func (c *Client) NewRequest(method, urlStr string, body interface{}, opts interface{}) (*http.Request, error) {
    u, err := c.baseURL.Parse(urlStr)
    if err != nil {
        return nil, err
    }

    // Add the options (page, per_page, ...) as query parameters. Fields with
    // the zero value are omitted, so PerPage == 0 never emits `per_page=0`.
    u, err = addOptions(u.String(), opts)
    if err != nil {
        return nil, err
    }
    // ... build request against u ...
}
```

3. Result: with `ListOptions{}` (both fields zero) the request URL carries no
   pagination query string at all; with `ListOptions{PerPage: 50}` it emits
   `?per_page=50`. Pagination becomes deterministic and matches GitHub's
   documented contract.

TESTS — github/github_test.go

```go
func TestNewRequest_Pagination(t *testing.T) {
    c := NewClient(nil)

    // Zero PerPage must NOT emit per_page.
    req, err := c.NewRequest("GET", "issues", nil, &ListOptions{})
    if err != nil {
        t.Fatalf("NewRequest returned error: %v", err)
    }
    if strings.Contains(req.URL.RawQuery, "per_page") {
        t.Errorf("zero PerPage should omit per_page, got query %q", req.URL.RawQuery)
    }

    // Explicit PerPage must be encoded.
    req, err = c.NewRequest("GET", "issues", nil, &ListOptions{PerPage: 50, Page: 2})
    if err != nil {
        t.Fatalf("NewRequest returned error: %v", err)
    }
    if !strings.Contains(req.URL.RawQuery, "per_page=50") {
        t.Errorf("expected per_page=50, got query %q", req.URL.RawQuery)
    }
    if !strings.Contains(req.URL.RawQuery, "page=2") {
        t.Errorf("expected page=2, got query %q", req.URL.RawQuery)
    }
}
```

This resolves the inconsistency deterministically: zero-value fields are omitted,
non-zero fields are encoded, and pagination behavior no longer depends on how a
particular endpoint interprets `per_page=0`.