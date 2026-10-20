package main

import (
	"os"
	"strings"
	"testing"

	"llm-proxy/pagegen"
)

// The embedded page is generated from admin.html + admin.js, so editing either
// source without re-running ./build.sh would ship a page that no longer matches
// the code under test. Comparing the checked-in file against a fresh render
// turns that into a failing test instead of a silent drift.
func TestAdminPageIsUpToDateWithItsSources(t *testing.T) {
	html, err := os.ReadFile("admin.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("admin.js")
	if err != nil {
		t.Fatal(err)
	}
	want, err := pagegen.Render(string(html), string(js))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(adminHTML) != want {
		t.Fatal("admin_page.html is stale: run ./build.sh")
	}
}

// The page must not reach outside itself: it is served on a network-facing port
// and a CDN or a second request would break it on an isolated host.
func TestAdminPageHasNoExternalReferences(t *testing.T) {
	page := string(adminHTML)
	for _, bad := range []string{"http://", "https://", "//cdn", "<link", "src="} {
		if strings.Contains(page, bad) {
			t.Errorf("the page must be self-contained, found %q", bad)
		}
	}
}

// The history section is the reason this page changed; if the generated file
// lost it, the chart would silently disappear from a working-looking page.
func TestAdminPageCarriesTheHistoryChart(t *testing.T) {
	page := string(adminHTML)
	for _, want := range []string{
		`id="history-controls"`,
		`id="history-chart"`,
		`id="history-summary"`,
		`id="history-tip"`,
		"/admin/api/stats/history",
		"历史速率",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}

// The admin page is four separate views, not one long scroll. If a refactor folds
// two back together, the operator loses the split this page exists to provide.
func TestAdminPageIsSplitIntoFourViews(t *testing.T) {
	page := string(adminHTML)
	for _, view := range []string{"usage", "errors", "main", "schemes"} {
		if !strings.Contains(page, `data-page="`+view+`"`) {
			t.Errorf("the page is missing the %q view", view)
		}
	}
	// Every listing that can grow without bound needs its own pager.
	for _, pager := range []string{"models-pager", "failures-pager", "routing-pager"} {
		if !strings.Contains(page, `id="`+pager+`"`) {
			t.Errorf("the page is missing the %q control", pager)
		}
	}
	// Each configuration view reports its own save result, so one page's message
	// cannot overwrite the other's.
	for _, bannerID := range []string{"main-banner", "scheme-banner"} {
		if !strings.Contains(page, `id="`+bannerID+`"`) {
			t.Errorf("the page is missing the %q region", bannerID)
		}
	}
}
