// Package agenttemplates embeds the default agent definitions so that
// `ai-squad init` can populate the agents directory from the binary itself.
package agenttemplates

import "embed"

// FS holds the starter agent definitions written by `ai-squad init`.
//
//go:embed *.yaml
var FS embed.FS
