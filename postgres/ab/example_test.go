package ab_test

import (
	"fmt"
	"github.com/alextanhongpin/dbtx/postgres/ab"
)

func ExampleInterpret() {
	summary, err := ab.Interpret(ab.Results{
		Control:   ab.Counts{Subjects: 1000, Conversions: 100},
		Treatment: ab.Counts{Subjects: 1000, Conversions: 200},
	}, .95)
	if err != nil {
		panic(err)
	}
	fmt.Println(summary.Conclusion)
	fmt.Printf("difference: %.2f; relative lift: %.2f\n", summary.Difference, *summary.RelativeLift)
	// Output:
	// treatment_better
	// difference: 0.10; relative lift: 1.00
}
