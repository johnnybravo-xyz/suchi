package presetfile

import (
	"fmt"
	"strings"
)

// PositionedError carries a best-effort file position alongside the
// message. Line/Col are 0 when the underlying parser or validator
// couldn't attribute a position.
type PositionedError struct {
	Line int
	Col  int
	Msg  string
}

func (e PositionedError) Error() string {
	if e.Line > 0 && e.Col > 0 {
		return fmt.Sprintf("line %d:%d: %s", e.Line, e.Col, e.Msg)
	}
	if e.Line > 0 {
		return fmt.Sprintf("line %d: %s", e.Line, e.Msg)
	}
	return e.Msg
}

// Errors is a slice of PositionedError implementing the `error`
// interface — every parse/validate entry returns this so callers can
// print all errors at once (spec §4: "returning a normalized preset
// or a list of positioned errors").
type Errors []PositionedError

func (es Errors) Error() string {
	if len(es) == 0 {
		return ""
	}
	if len(es) == 1 {
		return es[0].Error()
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// atPos is the internal builder used everywhere. Passing 0/0 = position
// unknown, still readable.
func atPos(line, col int, format string, args ...any) PositionedError {
	return PositionedError{Line: line, Col: col, Msg: fmt.Sprintf(format, args...)}
}

func at(line int, format string, args ...any) PositionedError {
	return atPos(line, 0, format, args...)
}

func noPos(format string, args ...any) PositionedError {
	return atPos(0, 0, format, args...)
}
