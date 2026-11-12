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

// Queries is docs/queries.md, served at /guide/queries.
//
//go:embed queries.md
var Queries string

// API is docs/api.md, served at /guide/api.
//
//go:embed api.md
var API string
