// Package configtemplate embeds the default configuration file so that
// `ai-squad init` can write a working setup without shipping loose files.
package configtemplate

import _ "embed"

// Default is the starter configuration written by `ai-squad init`.
//
//go:embed config.yaml
var Default []byte
