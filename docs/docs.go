// Package docs embeds the documentation that the web interface serves, so the
// binary still needs no files beside it.
package docs

import _ "embed"

// UserGuide is docs/user-guide.md, served at /guide.
//
//go:embed user-guide.md
var UserGuide string

// Collectors is docs/collectors.md, served at /guide/collectors.
//
//go:embed collectors.md
var Collectors string
