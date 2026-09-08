// Package migrations embeds the SQL schema migrations into the binary so that
// ai-squad can initialise a database without shipping loose files.
package migrations

import "embed"

// FS holds every migration, named NNNN_description.sql and applied in
// lexicographic order.
//
//go:embed *.sql
var FS embed.FS
