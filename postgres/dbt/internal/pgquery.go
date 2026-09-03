package internal

import (
	"fmt"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func ParseQuery(q string) (string, error) {
	stmt, err := pg_query.Parse(q)
	if err != nil {
		return "", fmt.Errorf("%w: parsing sql %q", err, q)
	}

	q, err = pg_query.Deparse(stmt)
	if err != nil {
		return "", fmt.Errorf("%w: deparsing sql %q", err, q)
	}

	return q, nil
}
