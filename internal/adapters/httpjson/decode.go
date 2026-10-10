package httpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"unicode/utf8"
)

func DecodeStrict(data []byte, value any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON value must be an object")
	}
	if !utf8.Valid(data) {
		return errors.New("JSON contains invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanValue(decoder, reflect.TypeOf(value)); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("JSON contains trailing data")
	}
	decoder = json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("JSON contains multiple values")
	}
	return nil
}

func scanValue(decoder *json.Decoder, valueType reflect.Type) error {
	for valueType != nil && valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return errors.New("JSON null is not allowed")
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}
	switch delimiter {
	case '{':
		fields := objectFields(valueType)
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("invalid JSON object key")
			}
			if _, found := seen[key]; found {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			fieldType, found := fields[key]
			if len(fields) > 0 && !found {
				return fmt.Errorf("unknown JSON field %q", key)
			}
			if err := scanValue(decoder, fieldType); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		var elementType reflect.Type
		if valueType != nil && (valueType.Kind() == reflect.Slice || valueType.Kind() == reflect.Array) {
			elementType = valueType.Elem()
		}
		for decoder.More() {
			if err := scanValue(decoder, elementType); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("unexpected JSON delimiter")
	}
}

func objectFields(valueType reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	if valueType == nil || valueType.Kind() != reflect.Struct {
		return fields
	}
	for index := 0; index < valueType.NumField(); index++ {
		field := valueType.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
