package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"

	"github.com/repogo/host/internal/sqlitedb"
)

// stateSchema is state.db's tables. Unlike the cache's schema, a change never
// deletes the file: openState adds what is new and refuses what differs.
//
//go:embed state.sql
var stateSchema string

// openState runs state.sql on the file at path, adds the columns it has gained,
// and refuses a file whose columns differ from it: those rows are the user's,
// so a mismatch stops the host rather than guessing.
func openState(path string) error {
	db := sqlitedb.Open(path, "PRAGMA synchronous=FULL", "PRAGMA busy_timeout=5000")
	defer db.Close()
	db.SetMaxOpenConns(1)

	want, err := stateShape()
	if err != nil {
		return err
	}
	if _, err := db.Exec(stateSchema); err != nil {
		return fmt.Errorf("run state.sql: %w", err)
	}
	have, err := tableShapes(db)
	if err != nil {
		return err
	}
	for table := range have {
		if _, ok := want[table]; !ok {
			return fmt.Errorf("state.db has table %s, which state.sql does not", table)
		}
	}
	for table, columns := range want {
		for _, column := range columns {
			got, ok := have[table][column.Name]
			switch {
			case !ok && (column.NotNull || column.PrimaryKey):
				return fmt.Errorf("state.sql adds %s.%s, which must be nullable", table, column.Name)
			case !ok:
				if _, err := db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + column.Name + ` ` + column.Type); err != nil {
					return fmt.Errorf("add %s.%s: %w", table, column.Name, err)
				}
			case got != column:
				return fmt.Errorf("state.db's %s.%s is %+v; state.sql says %+v", table, column.Name, got, column)
			}
		}
		for name := range have[table] {
			if _, ok := columns[name]; !ok {
				return fmt.Errorf("state.db has column %s.%s, which state.sql does not", table, name)
			}
		}
	}
	return nil
}

// stateColumn is what openState compares: a column's declared shape.
type stateColumn struct {
	Name       string
	Type       string
	NotNull    bool
	PrimaryKey bool
}

// stateShape is the tables state.sql makes, read from a scratch database so
// state.sql is the only description of them.
func stateShape() (map[string]map[string]stateColumn, error) {
	db := sqlitedb.Open(":memory:")
	defer db.Close()
	// Each connection to :memory: is its own database.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(stateSchema); err != nil {
		return nil, fmt.Errorf("run state.sql: %w", err)
	}
	return tableShapes(db)
}

// tableShapes is every table's columns by name.
func tableShapes(db *sql.DB) (map[string]map[string]stateColumn, error) {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return nil, err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make(map[string]map[string]stateColumn, len(tables))
	for _, table := range tables {
		columns, err := tableColumns(db, table)
		if err != nil {
			return nil, err
		}
		out[table] = columns
	}
	return out, nil
}

func tableColumns(db *sql.DB, table string) (map[string]stateColumn, error) {
	rows, err := db.Query(`SELECT name, type, "notnull", pk FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("columns of %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]stateColumn{}
	for rows.Next() {
		var c stateColumn
		var pk int
		if err := rows.Scan(&c.Name, &c.Type, &c.NotNull, &pk); err != nil {
			return nil, err
		}
		c.Type, c.PrimaryKey = strings.ToUpper(c.Type), pk > 0
		out[c.Name] = c
	}
	return out, rows.Err()
}

// attachState is the connect-hook statements that attach state.db as `state`
// on every connection the cache opens. A user's resolve or pick is
// acknowledged, so it is written with synchronous=FULL, unlike the cache.
func attachState(path string) []string {
	quoted := "'" + strings.ReplaceAll(path, "'", "''") + "'"
	return []string{
		"ATTACH DATABASE " + quoted + " AS state",
		"PRAGMA state.journal_mode=WAL",
		"PRAGMA state.synchronous=FULL",
	}
}
