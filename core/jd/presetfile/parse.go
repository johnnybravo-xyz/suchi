package presetfile

import (
	"bytes"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	huml "github.com/huml-lang/go-huml"
	"gopkg.in/yaml.v3"
)

// SerFormat is one of the accepted serializations.
type SerFormat string

const (
	FormatHuML SerFormat = "huml"
	FormatTOML SerFormat = "toml"
	FormatYAML SerFormat = "yaml"
)

// DetectFormat sniffs which serialization `data` is written in when
// the caller doesn't know (paste path). It looks for the earliest
// unambiguous marker: `%HUML` directive → HuML; `format =` → TOML;
// `format:` → YAML/HuML (defaults YAML for compat, since spec says
// HuML uses `%HUML` when embedded and both share `format:` syntax at
// the top level). Extension-driven callers should pass the format
// explicitly instead of relying on sniff — the sniff is a fallback.
func DetectFormat(data []byte) SerFormat {
	scan := bytes.TrimLeft(data, " \t\r\n")
	// %HUML directive is the definitive HuML marker.
	if bytes.HasPrefix(scan, []byte("%HUML")) {
		return FormatHuML
	}
	// Look for a definitive marker across the first non-empty non-comment
	// lines. `::` = HuML vector marker. `= ` at top-level with no `:`
	// on the same line = TOML scalar. First hit wins.
	lines := bytes.Split(scan, []byte("\n"))
	const maxScanLines = 20
	scanned := 0
	for _, ln := range lines {
		l := bytes.TrimSpace(ln)
		if len(l) == 0 || l[0] == '#' {
			continue
		}
		if bytes.Contains(l, []byte("::")) {
			return FormatHuML
		}
		if bytes.Contains(l, []byte("=")) && !bytes.Contains(l, []byte(":")) {
			return FormatTOML
		}
		scanned++
		if scanned >= maxScanLines {
			break
		}
	}
	// Fallback: treat as YAML (accepts a superset of many trivial
	// documents; strict decode still catches invalid ones).
	return FormatYAML
}

// Parse decodes `data` into a *PresetFile using the given serialization
// (or auto-detect if format == ""), then runs Validate.
//
// Returns (parsed, nil) on success, (nil, Errors) on any parse or
// validation failure. The two error kinds are combined into one Errors
// slice so callers surface them together — the presets-repo CI shows
// every problem in one CI run.
func Parse(data []byte, format SerFormat) (*PresetFile, error) {
	if len(data) == 0 {
		return nil, Errors{noPos("empty document")}
	}
	f := format
	if f == "" {
		f = DetectFormat(data)
	}
	var (
		pf   PresetFile
		errs Errors
	)
	switch f {
	case FormatHuML:
		if err := huml.Unmarshal(data, &pf); err != nil {
			errs = append(errs, humlError(err))
		}
	case FormatTOML:
		md, err := toml.Decode(string(data), &pf)
		if err != nil {
			errs = append(errs, tomlError(err))
		} else if u := md.Undecoded(); len(u) > 0 {
			for _, k := range u {
				errs = append(errs, noPos("unknown field %q", k))
			}
		}
	case FormatYAML:
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&pf); err != nil {
			errs = append(errs, yamlError(err))
		}
	default:
		return nil, Errors{noPos("unsupported format %q", string(f))}
	}
	if len(errs) > 0 {
		return nil, errs
	}
	if verrs := Validate(&pf); len(verrs) > 0 {
		return nil, verrs
	}
	return &pf, nil
}

// ParseFromExt is a convenience: pick the SerFormat from a filename
// extension. Unknown extension → FormatYAML (widest accept + strict
// decode).
func ParseFromExt(data []byte, ext string) (*PresetFile, error) {
	switch strings.ToLower(strings.TrimPrefix(ext, ".")) {
	case "huml":
		return Parse(data, FormatHuML)
	case "toml":
		return Parse(data, FormatTOML)
	case "yaml", "yml":
		return Parse(data, FormatYAML)
	default:
		return Parse(data, "")
	}
}

// humlError converts a raw parser error into PositionedError. go-huml
// error messages typically embed line numbers as `line N`; a small
// regex extracts them when present.
func humlError(err error) PositionedError {
	msg := err.Error()
	if line := extractLine(msg, "line "); line > 0 {
		return at(line, "huml: %s", msg)
	}
	return noPos("huml: %s", msg)
}

func tomlError(err error) PositionedError {
	// BurntSushi/toml embeds `(N, M)` position tuples in some errors.
	msg := err.Error()
	if line := extractLine(msg, "("); line > 0 {
		return at(line, "toml: %s", msg)
	}
	return noPos("toml: %s", msg)
}

func yamlError(err error) PositionedError {
	// yaml.v3 messages carry `line N:` prefixes.
	msg := err.Error()
	if line := extractLine(msg, "line "); line > 0 {
		return at(line, "yaml: %s", msg)
	}
	return noPos("yaml: %s", msg)
}

// extractLine best-effort pulls the first integer after `marker` from
// the message. Returns 0 when not found.
func extractLine(msg, marker string) int {
	i := strings.Index(msg, marker)
	if i < 0 {
		return 0
	}
	rest := msg[i+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
