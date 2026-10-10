package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"simulate", "-h"}, {"serve", "-h"}, {"report", "-h"}} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out, &out); err != nil || out.Len() == 0 {
			t.Fatal(args, err)
		}
	}
	for _, args := range [][]string{{"unknown"}, {"serve", "--address", "0.0.0.0:8080"}, {"simulate", "--probabilities", ".1,2"}, {"simulate", "--repetitions", "0"}, {"simulate", "--policies", "ucb1,ucb1"}} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out, &out); err == nil {
			t.Fatal(args)
		}
	}
}
func TestConnectionErrorsDoNotExposeCredentials(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://john:secret-password@[invalid")
	var out bytes.Buffer
	err := execute(t.Context(), []string{"report"}, &out, &out)
	if err == nil || strings.Contains(err.Error(), "secret-password") {
		t.Fatal(err)
	}
}
