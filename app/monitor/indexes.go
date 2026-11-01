// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"regexp"
	"strings"
)

// IndexSuggestion is an index a query shape could use: suggested, never created (the
// schema belongs in the app's migrations).
type IndexSuggestion struct {
	Table     string
	Columns   []string
	SQL       string
	Migration string // Laravel
	Note      string // what else the query needs, e.g. a range instead of DATE()
}

var (
	// col = ?, col IN (?), col > ?, col BETWEEN ?: equality first, then a range.
	eqRE    = regexp.MustCompile("(?i)(?:^|[\\s(,])`?((?:\\w+`?\\s*\\.\\s*`?)?\\w+)`?\\s*(?:=|\\bIN\\b)\\s*\\(?\\s*(?:\\?|\\$\\d+)")
	rangeRE = regexp.MustCompile("(?i)(?:^|[\\s(,])`?((?:\\w+`?\\s*\\.\\s*`?)?\\w+)`?\\s*(?:<=|>=|<|>|\\bBETWEEN\\b)\\s*(?:\\?|\\$\\d+)")
	dateRE  = regexp.MustCompile("(?i)\\b(?:DATE|YEAR|MONTH)\\s*`?\\s*\\(\\s*`?((?:\\w+`?\\s*\\.\\s*`?)?\\w+)`?\\s*\\)")
	colsRE  = regexp.MustCompile(`\(([^()]*)\)\s*(?:WHERE.*)?$`)
)

// indexColumns reads an index's columns from its definition: MySQL's "(a, b)" or
// PostgreSQL's "CREATE INDEX … USING btree (a, b)".
func indexColumns(def string) []string {
	m := colsRE.FindStringSubmatch(strings.TrimSpace(def))
	if m == nil {
		return nil
	}
	var out []string
	for _, c := range strings.Split(m[1], ",") {
		out = append(out, strings.ToLower(strings.Trim(strings.Fields(strings.TrimSpace(c) + " x")[0], "`\"")))
	}
	return out
}

func bare(col string) string {
	col = unquote(col)
	if i := strings.LastIndexByte(col, '.'); i >= 0 {
		col = col[i+1:]
	}
	return strings.ToLower(col)
}

// SuggestIndex proposes an index for a single-table query shape whose filter columns no
// index starts with. table is the table's name as the shape uses it.
func SuggestIndex(text, table string, indexes []IndexInfo) *IndexSuggestion {
	loc := whereRE.FindStringIndex(text)
	if loc == nil {
		return nil
	}
	where := text[loc[0]:]
	if i := regexp.MustCompile(`(?i)\b(GROUP BY|ORDER BY|LIMIT)\b`).FindStringIndex(where); i != nil {
		where = where[:i[0]]
	}
	skip := map[string]bool{"where": true, "and": true, "or": true, "on": true, "not": true}
	var cols []string
	seen := map[string]bool{}
	add := func(c string) {
		c = bare(c)
		if c != "" && !skip[c] && !seen[c] {
			seen[c] = true
			cols = append(cols, c)
		}
	}
	for _, m := range eqRE.FindAllStringSubmatch(where, -1) {
		add(m[1])
	}
	note := ""
	ranged := ""
	if m := dateRE.FindStringSubmatch(where); m != nil {
		ranged = bare(m[1])
		note = "Filter " + ranged + " with a range (>= the day's start and < the next day's) instead of " + strings.ToUpper(strings.Fields(m[0])[0]) + "(…): an index can't be used through the function."
	} else if m := rangeRE.FindStringSubmatch(where); m != nil {
		ranged = bare(m[1])
	}
	if ranged != "" && !seen[ranged] {
		cols = append(cols, ranged)
	}
	if len(cols) == 0 {
		return nil
	}
	for _, ix := range indexes {
		if c := indexColumns(ix.Definition); len(c) > 0 && c[0] == cols[0] {
			if note == "" {
				return nil // an index already starts with it
			}
			return &IndexSuggestion{Table: ix.Table, Note: note + " The index " + ix.Name + " on " + cols[0] + " can then be used."}
		}
	}
	name := "idx_" + bare(table) + "_" + strings.Join(cols, "_")
	if len(name) > 60 {
		name = name[:60]
	}
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = "'" + c + "'"
	}
	arg := quoted[0]
	if len(cols) > 1 {
		arg = "[" + strings.Join(quoted, ", ") + "]"
	}
	return &IndexSuggestion{Table: table, Columns: cols, Note: note,
		SQL:       "CREATE INDEX " + name + " ON " + table + " (" + strings.Join(cols, ", ") + ");",
		Migration: "Schema::table('" + bare(table) + "', function (Blueprint $table) {\n    $table->index(" + arg + ");\n});"}
}
