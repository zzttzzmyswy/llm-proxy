#!/bin/sh
# Inline admin.js into admin.html to produce admin_page.html, the single
# self-contained file the binary embeds. Run this after editing either source;
# admin_page_test.go fails when the generated file is stale.
set -eu
cd "$(dirname "$0")"
go run ./cmd/pagegen
