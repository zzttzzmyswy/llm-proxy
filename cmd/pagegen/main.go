// Command pagegen inlines admin.js into admin.html, producing the single
// self-contained admin_page.html that the binary embeds. The same rendering is
// used by admin_page_test.go, which fails when the checked-in file is stale.
package main

import (
	"os"

	"llm-proxy/pagegen"
)

func main() {
	html, err := os.ReadFile("admin.html")
	if err != nil {
		panic(err)
	}
	js, err := os.ReadFile("admin.js")
	if err != nil {
		panic(err)
	}
	page, err := pagegen.Render(string(html), string(js))
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile("admin_page.html", []byte(page), 0o644); err != nil {
		panic(err)
	}
}
