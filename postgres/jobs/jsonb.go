package jobs

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// validJSONB reports why b cannot be stored in a jsonb column, if at all.
// Postgres is stricter than json.Valid: it also rejects invalid UTF-8, lone
// surrogate escapes and \u0000. Values Postgres rejects for other reasons,
// such as numbers that overflow numeric, surface as ErrInvalidData.
func validJSONB(b []byte) error {
	if !json.Valid(b) {
		return errors.New("not valid JSON")
	}
	dec := jsontext.NewDecoder(bytes.NewReader(b))
	for {
		tok, err := dec.ReadToken()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if tok.Kind() == '"' && strings.ContainsRune(tok.String(), 0) {
			return errors.New(`contains \u0000, which jsonb does not support`)
		}
	}
}

// errText makes an error message storable in a text column: Postgres rejects
// invalid UTF-8 and NUL bytes. Long messages are truncated on a character
// boundary.
func errText(err error) string {
	const max = 2000
	s := strings.ToValidUTF8(err.Error(), "�")
	s = strings.ReplaceAll(s, "\x00", "")
	if len(s) > max {
		n := max
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n] + "…"
	}
	return s
}
