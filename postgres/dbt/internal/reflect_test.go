package internal

import "testing"

func TestReflect(t *testing.T) {
	t.Run("user", func(t *testing.T) {
		u := Make[User]()
		t.Logf("%#v\n", u)
		for _, v := range []any{u.Age, u.Company, u.Info} {
			if v == nil {
				t.Fatal("want non-nil")
			}
		}
	})

	t.Run("pointer user", func(t *testing.T) {
		u := Make[*User]()
		t.Logf("%#v\n", u)
		for _, v := range []any{u.Age, u.Company, u.Info} {
			if v == nil {
				t.Fatal("want non-nil")
			}
		}
	})

	t.Run("any", func(t *testing.T) {
		u := Make[any]()
		t.Logf("%#v\n", u)
		if u != nil {
			t.Fatal("want nil")
		}
	})
}

type Profile struct {
	Bio string
}

type User struct {
	Name    string
	Age     *int
	Company *string
	Tags    []string
	Info    *Profile
}
