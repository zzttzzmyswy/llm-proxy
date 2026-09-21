// Package pagegen builds the single self-contained admin page the binary
// embeds, by inlining the page's script into its markup.
//
// The page is authored as two files so the script can be linted and unit-tested
// under node (see admin_js_test.go), but it is served as one document with no
// external requests. Keeping the inlining in Go rather than in shell means the
// generator and the staleness test share exactly one implementation.
package pagegen

import (
	"fmt"
	"strings"
)

// Marker is the placeholder admin.html carries where the script goes.
const Marker = "/*__ADMIN_JS__*/"

// Render substitutes js into html at Marker.
//
// The script is inserted verbatim: a `</script>` inside it would close the tag
// early and silently truncate the page, so that is rejected rather than escaped.
func Render(html, js string) (string, error) {
	if !strings.Contains(html, Marker) {
		return "", fmt.Errorf("pagegen: %s not found in the template", Marker)
	}
	if strings.Contains(js, "</script") {
		return "", fmt.Errorf("pagegen: the script contains a closing script tag")
	}
	return strings.Replace(html, Marker, strings.TrimRight(js, "\n"), 1), nil
}
