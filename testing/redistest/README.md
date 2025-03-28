# redistest

`redistest` is a testing utility package for working with Redis in Go. It provides helpers to set up and tear down Redis instances for integration tests, ensuring a clean and isolated environment for each test.

## Installation

```bash
go get github.com/alextanhongpin/dbtx/testing/redistest
```

## Usage

Below is an example of how to use the `redistest` package in your tests:

```go
package yourpackage_test

import (
	"context"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/redistest"
)

func TestRedisIntegration(t *testing.T) {
	// Start a Redis instance for testing
	db := redistest.New(t).Client(t)

	// Use the client in your tests
	ctx := context.Background()
	err := db.Set(ctx, "key", "value", 0).Err()
	if err != nil {
		t.Fatalf("failed to set key: %v", err)
	}

	val, err := db.Get(ctx, "key").Result()
	if err != nil {
		t.Fatalf("failed to get key: %v", err)
	}

	if val != "value" {
		t.Fatalf("expected value to be 'value', got '%s'", val)
	}
}
```

## Setting Up Global Test in `main`

You can also set up a global Redis instance for all tests in your package by using the `TestMain` function:

```go
package yourpackage_test

import (
	"context"
	"os"
	"testing"

	"github.com/alextanhongpin/dbtx/testing/redistest"
)

var stop func() error

func TestMain(m *testing.M) {
	// Initialize the global Redis instance
	stop = redistest.Init()
	defer stop()

	// Run tests
	m.Run()
}

func TestGlobalRedis(t *testing.T) {
	// Create a Redis client using the global instance
	db := redistest.Client(t)

	// Use the client in your tests
	ctx := context.Background()
	err := db.Set(ctx, "globalKey", "globalValue", 0).Err()
	if err != nil {
		t.Fatalf("failed to set key: %v", err)
	}

	val, err := db.Get(ctx, "globalKey").Result()
	if err != nil {
		t.Fatalf("failed to get key: %v", err)
	}

	if val != "globalValue" {
		t.Fatalf("expected value to be 'globalValue', got '%s'", val)
	}
}
```

## Features

- Start and stop Redis instances for testing.
- Automatically clean up resources after tests.
- Compatible with `go-redis`.

## License

This package is licensed under the MIT License.
