package main

import (
	_ "embed"
	"net/http"
)

// admin_page.html is generated from admin.html + admin.js by ./build.sh. The
// binary serves one self-contained document: the page is reachable on a
// network-facing port, so it must not depend on a CDN or any second request.
//
//go:embed admin_page.html
var adminHTML []byte

func handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(adminHTML)
}
