package internal

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// ParseQuery validates q with the real Postgres parser and returns it in
// normalized form. Invalid SQL is therefore reported when the query is
// compiled (usually at program start), not on first execution.
func ParseQuery(q string) (string, error) {
	tree, err := pg_query.Parse(q)
	if err != nil {
		return "", fmt.Errorf("parsing sql %q: %w", q, err)
	}
	out, err := pg_query.Deparse(tree)
	if err != nil {
		// Report the original query; the old code reported the (empty)
		// result of the failed Deparse by shadowing q.
		return "", fmt.Errorf("deparsing sql %q: %w", q, err)
	}
	return out, nil
}
