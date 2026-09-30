// Copyright 2026 The go-github Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Regression tests for bounty issue #5:
// "Fix pagination inconsistency and parameter handling when
//  ListOptions.PerPage is 0".
//
// These tests lock in the required behavior:
//   1. ListOptions{PerPage: 0} must NOT emit an invalid/duplicate per_page
//      query parameter (omitempty must behave correctly).
//   2. Response pagination helpers (NextPage, PrevPage, FirstPage, LastPage)
//      must extract page numbers from the Link header regardless of whether
//      per_page was explicitly provided or defaulted by the server.
//   3. Rebuilding a request URL / reusing ListOptions across successive page
//      calls must retain expected query parameters (no unintended drops).
//   4. Existing endpoints embedding ListOptions must remain backwards
//      compatible.

package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// TestAddOptions_PerPageZero verifies that PerPage == 0 omits the per_page
// query parameter entirely (relying on the server default) and that an
// explicit PerPage is encoded exactly once.
func TestAddOptions_PerPageZero(t *testing.T) {
	tests := []struct {
		name string
		opts *ListOptions
		want string
	}{
		{"zero per page omits param", &ListOptions{Page: 1, PerPage: 0}, "page=1"},
		{"explicit per page encoded", &ListOptions{Page: 2, PerPage: 50}, "page=2&per_page=50"},
		{"zero page and per page", &ListOptions{Page: 0, PerPage: 0}, ""},
		{"only per page", &ListOptions{Page: 0, PerPage: 100}, "per_page=100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, err := url.Parse("https://api.github.com/repos/o/r/issues")
			if err != nil {
				t.Fatalf("url.Parse: %v", err)
			}
			if err := addOptions(u, tt.opts); err != nil {
				t.Fatalf("addOptions: %v", err)
			}
			if got := u.RawQuery; got != tt.want {
				t.Errorf("RawQuery = %q, want %q", got, tt.want)
			}
			// Guard against the classic bug: per_page appearing twice.
			if q := u.Query(); len(q["per_page"]) > 1 {
				t.Errorf("per_page encoded %d times, want <= 1", len(q["per_page"]))
			}
		})
	}
}

// TestParsePagination_PerPageOmitted verifies that page-number helpers are
// populated from the Link header whether or not per_page is present in the
// server-returned URLs.
func TestParsePagination_PerPageOmitted(t *testing.T) {
	tests := []struct {
		name  string
		link  string
		next  int
		prev  int
		first int
		last  int
	}{
		{
			name: "no per_page in link relations",
			link: `<https://api.github.com/resource?page=2>; rel="next", ` +
				`<https://api.github.com/resource?page=1>; rel="prev", ` +
				`<https://api.github.com/resource?page=1>; rel="first", ` +
				`<https://api.github.com/resource?page=5>; rel="last"`,
			next: 2, prev: 1, first: 1, last: 5,
		},
		{
			name: "explicit per_page in link relations",
			link: `<https://api.github.com/resource?page=3&per_page=50>; rel="next", ` +
				`<https://api.github.com/resource?page=1&per_page=50>; rel="prev", ` +
				`<https://api.github.com/resource?page=1&per_page=50>; rel="first", ` +
				`<https://api.github.com/resource?page=9&per_page=50>; rel="last"`,
			next: 3, prev: 1, first: 1, last: 9,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &Response{}
			parsePagination(resp, http.Header{"Link": []string{tt.link}})

			if resp.NextPage != tt.next {
				t.Errorf("NextPage = %d, want %d", resp.NextPage, tt.next)
			}
			if resp.PrevPage != tt.prev {
				t.Errorf("PrevPage = %d, want %d", resp.PrevPage, tt.prev)
			}
			if resp.FirstPage != tt.first {
				t.Errorf("FirstPage = %d, want %d", resp.FirstPage, tt.first)
			}
			if resp.LastPage != tt.last {
				t.Errorf("LastPage = %d, want %d", resp.LastPage, tt.last)
			}
		})
	}
}

// TestListOptions_PerPageZeroAcrossPages is an end-to-end mock test: it drives
// a multi-page traversal with PerPage left at the zero value and asserts that
// every page is fetched exactly once and in order, with no dropped or
// duplicated query parameters.
func TestListOptions_PerPageZeroAcrossPages(t *testing.T) {
	var requested []string
	mux := http.NewServeMux()
	mux.HandleFunc("/resource", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		requested = append(requested, page)

		// Reject an invalid/duplicated per_page parameter outright.
		if v := r.URL.Query()["per_page"]; len(v) > 1 {
			http.Error(w, "duplicate per_page", http.StatusBadRequest)
			return
		}

		base := fmt.Sprintf("http://%s/resource", r.Host)
		var link string
		switch page {
		case "1":
			link = fmt.Sprintf(`<%s?page=2>; rel="next", <%s?page=3>; rel="last"`, base, base)
		case "2":
			link = fmt.Sprintf(`<%s?page=1>; rel="prev", <%s?page=3>; rel="next", <%s?page=3>; rel="last"`, base, base, base)
		case "3":
			link = fmt.Sprintf(`<%s?page=2>; rel="prev", <%s?page=1>; rel="first"`, base, base)
		}
		if link != "" {
			w.Header().Set("Link", link)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":%s}]`, page)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(nil)
	client.BaseURL, _ = url.Parse(srv.URL + "/")

	opts := &ListOptions{Page: 1, PerPage: 0} // PerPage left at zero value
	var pages int

	for {
		_, resp, err := client.Do(nil, "GET", "resource", opts, nil)
		if err != nil {
			t.Fatalf("page %d: Do: %v", opts.Page, err)
		}
		pages++
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	if pages != 3 {
		t.Fatalf("traversed %d pages, want 3 (requested=%v)", pages, requested)
	}
	want := []string{"1", "2", "3"}
	for i, p := range want {
		if requested[i] != p {
			t.Errorf("request %d = page %q, want %q", i, requested[i], p)
		}
	}
}
