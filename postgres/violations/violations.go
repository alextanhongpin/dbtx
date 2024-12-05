/*
	Postgres error handling.
	Reference: https://www.postgresql.org/docs/9.2/errcodes-appendix.html

	We want to handle the following codes

	23000	integrity_constraint_violation
	23001	restrict_violation
	23502	not_null_violation
	23503	foreign_key_violation
	23505	unique_violation
	23514	check_violation
	23P01	exclusion_violation
*/

package violations

import (
	"errors"

	"github.com/lib/pq"
)

const (
	IntegrityConstraint = "23000"
	Restrict            = "23001"
	NotNull             = "23502"
	ForeignKey          = "23503"
	Unique              = "23505"
	Check               = "23514"
	Exclusion           = "23P01"
	TriggerException    = "P0000"
)

func As(err error) (*pq.Error, bool) {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return pqErr, true
	}

	return nil, false
}

func IsCode(err error, code string) bool {
	pqErr, ok := As(err)
	return ok && pqErr.Code == pq.ErrorCode(code)
}

func IsIntegrityConstraint(err error) bool {
	return IsCode(err, IntegrityConstraint)
}

func IsRestrict(err error) bool {
	return IsCode(err, Restrict)
}

func IsNotNull(err error) bool {
	return IsCode(err, NotNull)
}

// IsForeignKey can be used to test if the row to hard delete contains
// foreign keys references.
// If this error is detected, then just soft delete.
func IsForeignKey(err error) bool {
	return IsCode(err, ForeignKey)
}

func IsUnique(err error) bool {
	return IsCode(err, Unique)
}

func IsCheck(err error) bool {
	return IsCode(err, Check)
}

func IsExclusion(err error) bool {
	return IsCode(err, Exclusion)
}

func IsTriggerException(err error) bool {
	return IsCode(err, TriggerException)
}
