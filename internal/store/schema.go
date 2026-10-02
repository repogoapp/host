package store

import _ "embed"

// schema is the cache's tables, shared with the Rust importer (store.rs), so
// a database written by either opens in the other. Any change resets the cache.
//
//go:embed schema.sql
var schema string
