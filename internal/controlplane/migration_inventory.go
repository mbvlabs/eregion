package controlplane

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/dbschema"
)

var migrationCreate = regexp.MustCompile(`(?s)^CREATE (TABLE|(?:UNIQUE )?INDEX) (eregion_[a-z0-9_]+)\b`)
var migrationIndexOwner = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (eregion_[a-z0-9_]+)\s+ON (eregion_[a-z0-9_]+)\s*\(`)
var migrationComment = regexp.MustCompile(`(?m)--[^\n]*`)

// applicationInventory is deliberately limited to this CREATE-only history.
// Unknown statement forms fail closed rather than silently expanding adoption.
func applicationInventory(current int) (tables, indexes []string, err error) {
	files, err := fs.Glob(dbschema.GooseFS, "migrations/*.sql")
	if err != nil || current < 0 || current > len(files) {
		return nil, nil, fmt.Errorf("invalid application inventory prefix")
	}
	tables, indexes = []string{}, []string{}
	seen := map[string]bool{}
	for _, file := range files[:current] {
		data, err := fs.ReadFile(dbschema.GooseFS, file)
		if err != nil {
			return nil, nil, err
		}
		for _, statement := range strings.Split(migrationComment.ReplaceAllString(string(data), ""), ";") {
			table, index, err := applicationInventoryStatement(statement, seen)
			if err != nil {
				return nil, nil, err
			}
			if table != "" {
				tables = append(tables, table)
			}
			if index != "" {
				indexes = append(indexes, index)
			}
		}
	}
	return tables, indexes, nil
}

// applicationInventoryStatement classifies one CREATE-only migration
// statement. Index ownership is emitted as "index:table" for inventory checks.
func applicationInventoryStatement(statement string, seen map[string]bool) (table, index string, err error) {
	statement = strings.TrimSpace(statement)
	if statement == "" {
		return "", "", nil
	}
	match := migrationCreate.FindStringSubmatch(statement)
	if match == nil || seen[match[2]] {
		return "", "", fmt.Errorf("unsupported application migration statement")
	}
	seen[match[2]] = true
	if match[1] == "TABLE" {
		return match[2], "", nil
	}
	owner := migrationIndexOwner.FindStringSubmatch(statement)
	if owner == nil || !seen[owner[2]] {
		return "", "", fmt.Errorf("unsupported application index owner")
	}
	return "", owner[1] + ":" + owner[2], nil
}
