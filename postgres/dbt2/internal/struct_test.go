package internal

import (
	"testing"
	"time"
)

func TestStruct(t *testing.T) {
	type Book struct {
		Title       string
		PublishedAt time.Time
	}

	type Author struct {
		Name string
	}

	type BookAuthor struct {
		Author
		Book
	}
	type BookAuthorNested struct {
		Author Author
		Book   Book
	}
	type BookAuthorEmbeddedPointer struct {
		*Author
		*Book
	}

	t.Log(StructFieldsType[BookAuthor]())
	t.Log(StructFieldsType[*BookAuthor]())
	t.Log(StructFieldsType[BookAuthorNested]())
	t.Log(StructFieldsType[*BookAuthorNested]())
	t.Log(StructFieldsType[BookAuthorEmbeddedPointer]())
	t.Log(StructFieldsType[*BookAuthorEmbeddedPointer]())
}
