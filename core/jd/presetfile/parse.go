// SPDX-License-Identifier: AGPL-3.0-or-later

package presetfile

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"
)

type SerFormat string

const (
	FormatHuML  SerFormat = "huml"
	FormatTOML  SerFormat = "toml"
	MaxFileSize           = 1 << 20
)

// Parse validates author input before expanding flat categories or generating Inbox.
// An omitted serialization means HuML; content is never sniffed.
func Parse(data []byte, format SerFormat) (*PresetFile, error) {
	if len(data) == 0 {
		return nil, Errors{noPos("empty document")}
	}
	if len(data) > MaxFileSize {
		return nil, Errors{noPos("document exceeds %d bytes", MaxFileSize)}
	}
	if !utf8.Valid(data) {
		return nil, Errors{noPos("document must be valid UTF-8")}
	}
	var raw map[string]any
	switch format {
	case "", FormatHuML:
		if err := huml.Unmarshal(data, &raw); err != nil {
			return nil, Errors{syntaxError("huml", err)}
		}
	case FormatTOML:
		if _, err := toml.Decode(string(data), &raw); err != nil {
			return nil, Errors{syntaxError("toml", err)}
		}
	default:
		return nil, Errors{noPos("unsupported serialization %q; use huml or toml", format)}
	}
	// Format dispatch precedes v1 shape diagnostics, including unknown keys.
	if raw["format"] != Format {
		return nil, Errors{noPos("format must be %q, got %v", Format, raw["format"])}
	}
	var pf PresetFile
	if err := decodeRaw(reflect.ValueOf(&pf).Elem(), raw, ""); err != nil {
		return nil, Errors{noPos("%s", err)}
	}
	if value, present := raw["system"]; present && value == "" {
		return nil, Errors{noPos("system: must be nonempty when supplied")}
	}
	if pf.Flat {
		if _, ok := raw["areas"]; ok {
			return nil, Errors{noPos("areas: forbidden when flat is true")}
		}
		// Zero is a missing-code sentinel only for in-memory input, not an explicit raw code.
		if list, ok := raw["categories"]; ok {
			v := reflect.ValueOf(list)
			for i := 0; i < v.Len(); i++ {
				m := v.Index(i).Interface().(map[string]any)
				if _, present := m["code"]; present && pf.Categories[i].Code != 11+i {
					return nil, Errors{noPos("categories[%d].code: must be %d", i, 11+i)}
				}
			}
		}
	} else if _, ok := raw["categories"]; ok {
		return nil, Errors{noPos("categories: forbidden unless flat is true")}
	}
	if es := Validate(&pf); len(es) > 0 {
		return nil, es
	}
	if pf.Flat {
		cats := append([]Category(nil), pf.Categories...)
		for i := range cats {
			cats[i].Code = 11 + i
		}
		pf.Areas = []Area{{Code: 10, Name: pf.Name, Categories: cats}}
		pf.Categories = nil
	}
	pf.Areas = append(pf.Areas, Area{Code: 40, Name: "System", Categories: []Category{{Code: 49, Name: "Inbox"}}})
	pf.Inbox = 49
	return &pf, nil
}

// decodeRaw uses the public schema's tags but never performs scalar coercion.
// Both parsers reject duplicate dictionary keys, including nested inline maps.
func decodeRaw(dst reflect.Value, src any, path string) error {
	if src == nil {
		return fmt.Errorf("%s: null is not supported", path)
	}
	if dst.Kind() == reflect.Pointer {
		dst.Set(reflect.New(dst.Type().Elem()))
		return decodeRaw(dst.Elem(), src, path)
	}
	switch dst.Kind() {
	case reflect.Struct:
		m, ok := src.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object", path)
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			field := -1
			for i := 0; i < dst.NumField(); i++ {
				if strings.Split(dst.Type().Field(i).Tag.Get("json"), ",")[0] == k && k != "-" {
					field = i
					break
				}
			}
			p := k
			if path != "" {
				p = path + "." + k
			}
			if field < 0 {
				return fmt.Errorf("%s: unknown field", p)
			}
			if err := decodeRaw(dst.Field(field), m[k], p); err != nil {
				return err
			}
		}
	case reflect.Slice:
		v := reflect.ValueOf(src)
		if v.Kind() != reflect.Slice {
			return fmt.Errorf("%s: expected list", path)
		}
		dst.Set(reflect.MakeSlice(dst.Type(), v.Len(), v.Len()))
		for i := 0; i < v.Len(); i++ {
			if err := decodeRaw(dst.Index(i), v.Index(i).Interface(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		m, ok := src.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: expected object", path)
		}
		dst.Set(reflect.ValueOf(m))
	case reflect.String:
		v, ok := src.(string)
		if !ok {
			return fmt.Errorf("%s: expected string", path)
		}
		dst.SetString(v)
	case reflect.Bool:
		v, ok := src.(bool)
		if !ok {
			return fmt.Errorf("%s: expected boolean", path)
		}
		dst.SetBool(v)
	case reflect.Int:
		v, ok := src.(int64)
		if !ok || dst.OverflowInt(v) {
			return fmt.Errorf("%s: expected integer", path)
		}
		dst.SetInt(v)
	default:
		return fmt.Errorf("%s: unsupported value", path)
	}
	return nil
}

func ParseFromExt(data []byte, ext string) (*PresetFile, error) {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "huml":
		return Parse(data, FormatHuML)
	case "toml":
		return Parse(data, FormatTOML)
	default:
		return nil, Errors{noPos("unsupported extension %q; use .huml or .toml", ext)}
	}
}

// Marshal reverses normalization, serializes the raw authoring shape, and
// revalidates the exact bytes. It never silently drops legacy reserved content.
func Marshal(pf *PresetFile, format SerFormat) ([]byte, error) {
	raw, err := rawFromNormalized(pf)
	if err != nil {
		return nil, err
	}
	// A map keeps areas explicitly empty for blank trees, but absent for flat ones.
	m := map[string]any{"format": raw.Format, "id": raw.ID, "version": raw.Version, "name": raw.Name, "market": raw.Market, "language": raw.Language, "story": raw.Story}
	if raw.System != "" {
		m["system"] = raw.System
	}
	if raw.Maintainer != "" {
		m["maintainer"] = raw.Maintainer
	}
	if raw.License != "" {
		m["license"] = raw.License
	}
	if raw.Flat {
		m["flat"] = true
		m["categories"] = raw.Categories
	} else {
		m["areas"] = raw.Areas
	}
	if raw.Seeds != nil {
		m["seeds"] = raw.Seeds
	}
	var data []byte
	switch format {
	case "", FormatHuML:
		data, err = huml.Marshal(m)
	case FormatTOML:
		var b bytes.Buffer
		err = toml.NewEncoder(&b).Encode(m)
		data = b.Bytes()
	default:
		return nil, Errors{noPos("unsupported serialization %q; use huml or toml", format)}
	}
	if err != nil {
		return nil, err
	}
	if _, err := Parse(data, format); err != nil {
		return nil, err
	}
	return data, nil
}

func syntaxError(format string, err error) PositionedError {
	var tomlErr toml.ParseError
	if errors.As(err, &tomlErr) {
		return atPos(tomlErr.Position.Line, tomlErr.Position.Col, "%s: %s", format, tomlErr.Message)
	}
	msg := err.Error()
	for _, marker := range []string{"line ", "("} {
		if i := strings.Index(msg, marker); i >= 0 {
			rest := msg[i+len(marker):]
			end := 0
			for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
				end++
			}
			if n, e := strconv.Atoi(rest[:end]); e == nil {
				return at(n, "%s: %s", format, msg)
			}
		}
	}
	return noPos("%s: %s", format, msg)
}
