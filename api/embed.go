// Package apidocs embeds the same API contract shipped in the repository.
package apidocs

import _ "embed"

//go:embed openapi.yaml
var OpenAPI []byte

//go:embed guide.md
var Guide []byte
