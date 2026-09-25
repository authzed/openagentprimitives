// Package dotenv provides a minimal .env-file reader used by oap secret-writing
// commands (oap identity put-token, oap agent put-key).
package dotenv

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// Read opens the file at path, scans for a line whose key equals key (using
// the form KEY=VALUE), and returns the value bytes. Surrounding double or
// single quotes are stripped (one layer). Blank lines and comment lines
// (starting with '#') are skipped. Values may themselves contain '='.
//
// Errors name what was tried, e.g.:
//
//	dotenv: read .env: open .env: no such file or directory
//	dotenv: key "FOO" not found in .env
func Read(path, key string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("dotenv: read %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip blank lines and comments.
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Anchor on KEY= at the start of the trimmed line.
		prefix := key + "="
		if !strings.HasPrefix(trimmed, prefix) {
			continue
		}
		value := trimmed[len(prefix):]
		value = stripSurroundingQuotes(value)
		return []byte(value), nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("dotenv: read %s: %w", path, err)
	}
	return nil, fmt.Errorf("dotenv: key %q not found in %s", key, path)
}

// stripSurroundingQuotes removes one layer of surrounding double or single
// quotes from v, if present (both must match).
func stripSurroundingQuotes(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') ||
			(v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}
