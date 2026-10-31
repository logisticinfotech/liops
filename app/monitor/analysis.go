// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"regexp"
	"strings"
)

// Flag is a pattern in a query that stops a database from using an index, with what to
// do about it.
type Flag struct {
	Name string `json:"name"`
	Fix  string `json:"fix"`
}

var (
	// A column inside a function in a comparison: DATE(created_at) = ?, LOWER(name) LIKE ?.
	// MySQL's digests put spaces around parentheses and quote names with backticks.
	funcOnColumnRE = regexp.MustCompile("(?i)\\b(DATE|LOWER|UPPER|YEAR|MONTH|DAY|DATE_FORMAT|SUBSTRING|SUBSTR|LEFT|TRIM|CAST|CONVERT|COALESCE|IFNULL|CONCAT|UNIX_TIMESTAMP|TO_CHAR)\\s*\\(\\s*((?:[`\"]?\\w+[`\"]?\\s*\\.\\s*)?[`\"]?[A-Za-z_]\\w*[`\"]?)\\s*(?:,[^()]*)?\\)\\s*(?:::\\s*\\w+\\s*)?(=|<=|>=|<>|!=|<|>|NOT\\s+LIKE|LIKE|ILIKE|IN\\b|BETWEEN)")
	// col = ? OR other = ?
	orAcrossRE    = regexp.MustCompile("(?i)([`\"\\w.]+)\\s*(?:=|LIKE|ILIKE)\\s*(?:\\?|\\$\\d+)\\s*\\)?\\s+OR\\s+\\(?\\s*([`\"\\w.]+)\\s*(?:=|LIKE|ILIKE)")
	selectStarRE  = regexp.MustCompile("(?i)^\\s*SELECT\\s+(?:[`\"\\w]+\\s*\\.\\s*)?\\*\\s+FROM")
	leadingLikeRE = regexp.MustCompile(`(?i)\bLIKE\s+(?:_\w+\s*)?'%`)
	tablesRE      = regexp.MustCompile("(?i)\\b(?:FROM|JOIN|UPDATE|INTO)\\s+([`\"]?[\\w$]+[`\"]?(?:\\s*\\.\\s*[`\"]?[\\w$]+[`\"]?)?)")
	whereRE       = regexp.MustCompile(`(?i)\b(WHERE|ON|HAVING)\b`)
)

// QueryFlags finds the patterns in a query shape that defeat indexes.
func QueryFlags(text string, c QueryCounters) []Flag {
	var out []Flag
	if loc := whereRE.FindStringIndex(text); loc != nil {
		cond := text[loc[0]:]
		if m := funcOnColumnRE.FindStringSubmatch(cond); m != nil {
			col := unquote(m[2])
			fn := strings.ToUpper(m[1])
			fix := "Compare the bare column, so its index can be used"
			switch fn {
			case "DATE":
				fix = "Use a range instead: " + col + " >= '2026-01-01' AND " + col + " < '2026-01-02'"
			case "YEAR", "MONTH", "DAY":
				fix = "Use a date range on " + col + " instead"
			case "LOWER", "UPPER":
				fix = "Store and compare the value in one case, or use a case-insensitive collation (MySQL's default is)"
			}
			out = append(out, Flag{Name: fn + "(" + col + ") in a filter: no index on " + col + " can be used", Fix: fix + "."})
		}
		if m := orAcrossRE.FindStringSubmatch(cond); m != nil && unquote(m[1]) != unquote(m[2]) {
			out = append(out, Flag{Name: "OR across columns (" + unquote(m[1]) + ", " + unquote(m[2]) + ")",
				Fix: "One index can't serve both sides: split it into a UNION of two queries, each with its own index, or search a combined column."})
		}
	}
	if selectStarRE.MatchString(text) && c.Calls > 0 && c.RowsSent/c.Calls >= 1000 {
		out = append(out, Flag{Name: "SELECT * returning many rows",
			Fix: "Select only the columns the page uses, and page through the rows (LIMIT)."})
	}
	return out
}

// SampleFlags finds what only a real statement shows: a LIKE starting with a wildcard
// (the shape has LIKE ? there).
func SampleFlags(sample string) []Flag {
	if leadingLikeRE.MatchString(sample) {
		return []Flag{{Name: "LIKE '%…' (a leading wildcard): every row is read",
			Fix: "A normal index can't help a search inside text. Search from the start ('term%'), use a FULLTEXT index, or a search engine; and limit how often it runs (e.g. not on every keystroke)."}}
	}
	return nil
}

// Efficiency is how much a query reads for what it returns.
type Efficiency struct {
	// Ratio is rows examined per row sent (MySQL), or 8 KB blocks read per row returned
	// (PostgreSQL). 0 when nothing was returned.
	Ratio float64 `json:"ratio"`
	// PerCall is rows examined (MySQL) or blocks read (PostgreSQL) per call.
	PerCall float64 `json:"per_call"`
	// Scans: it reads far more than it returns, the mark of a scan with no usable index.
	Scans bool `json:"scans"`
}

// QueryEfficiency judges a query shape's numbers. engine is EngineMySQL or EnginePostgres.
func QueryEfficiency(engine string, c QueryCounters) Efficiency {
	var e Efficiency
	if c.Calls <= 0 {
		return e
	}
	e.PerCall = c.RowsExamined / c.Calls
	if c.RowsSent > 0 {
		e.Ratio = c.RowsExamined / c.RowsSent
	}
	if engine == EngineMySQL {
		noIndex := c.NoIndex / c.Calls
		e.Scans = e.PerCall >= 10000 && (c.RowsSent == 0 || e.Ratio >= 100) || noIndex >= 0.5 && e.PerCall >= 1000
	} else {
		// 1,250 blocks is 10 MB read per call.
		e.Scans = e.PerCall >= 1250 && (c.RowsSent == 0 || e.Ratio >= 10)
	}
	return e
}

// TablesIn lists the tables a query shape names (FROM, JOIN, UPDATE, INTO), unquoted:
// "orders" or "public.orders".
func TablesIn(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tablesRE.FindAllStringSubmatch(text, -1) {
		t := unquote(m[1])
		if l := strings.ToLower(t); l == "select" || l == "dual" || l == "lateral" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// unquote removes `quotes` and "quotes" and the spaces MySQL's digests put around dots.
func unquote(s string) string {
	return strings.NewReplacer("`", "", `"`, "", " ", "").Replace(s)
}
