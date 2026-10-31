package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const jsonFieldVersion = "version"

func rejectTrailingJSON(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

// DecodeStrictJSON decodes exactly one JSON value into target, rejecting
// invalid UTF-8, duplicate or unknown fields, unpaired surrogate escapes and
// trailing data.
func DecodeStrictJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("JSON is not valid UTF-8")
	}
	if err := rejectDuplicateResourceFields(data); err != nil {
		return err
	}
	if err := rejectUnpairedJSONSurrogates(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
