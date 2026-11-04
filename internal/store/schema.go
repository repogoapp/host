package store

import _ "embed"

// schema is the cache's tables. Any change resets the cache.
//
//go:embed schema.sql
var schema string
