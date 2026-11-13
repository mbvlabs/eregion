package controlplane

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/dbschema"
)

var migrationCreate = regexp.MustCompile(`(?s)^CREATE (TABLE|(?:UNIQUE )?INDEX) (eregion_[a-z0-9_]+)\b`)
var migrationIndexOwner = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (eregion_[a-z0-9_]+) ON (eregion_[a-z0-9_]+)\s*\(`)
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
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			match := migrationCreate.FindStringSubmatch(statement)
			if match == nil || seen[match[2]] {
				return nil, nil, fmt.Errorf("unsupported application migration statement")
			}
			seen[match[2]] = true
			if match[1] == "TABLE" {
				tables = append(tables, match[2])
			} else {
				index := migrationIndexOwner.FindStringSubmatch(statement)
				if index == nil || !seen[index[2]] {
					return nil, nil, fmt.Errorf("unsupported application index owner")
				}
				indexes = append(indexes, index[1]+":"+index[2])
			}
		}
	}
	return tables, indexes, nil
}
