// Package errors stellt einheitliche, pluginübergreifende Fehler bereit.
package errors

import "fmt"

// Code klassifiziert Fehler für CLI/REST/MCP-Mapping.
type Code string

const (
	CodeNotFound     Code = "NOT_FOUND"
	CodeUnauthorized Code = "UNAUTHORIZED"
	CodeBadRequest   Code = "BAD_REQUEST"
	CodeUpstream     Code = "UPSTREAM_ERROR"
	CodeInternal     Code = "INTERNAL"
)

// Error ist der Core-Fehlertyp mit Plugin-Kontext.
type Error struct {
	Code    Code
	Plugin  string
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Plugin != "" {
		return fmt.Sprintf("[%s/%s] %s", e.Plugin, e.Code, e.Message)
	}
	return fmt.Sprintf("[%s] %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func New(code Code, plugin, msg string) *Error {
	return &Error{Code: code, Plugin: plugin, Message: msg}
}

func Wrap(code Code, plugin, msg string, err error) *Error {
	return &Error{Code: code, Plugin: plugin, Message: msg, Err: err}
}
