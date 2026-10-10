// Package validate shares identity validation between the API and repository.
package validate

import (
	"fmt"
	"strings"
)

// ID rejects blank identities without changing their contents.
func ID(kind, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("ab: %s ID is required", kind)
	}
	return nil
}

func Subject(experiment, subject string) error {
	if err := ID("experiment", experiment); err != nil {
		return err
	}
	return ID("subject", subject)
}
