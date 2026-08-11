package harbor

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means the project is confirmed absent: a name lookup
	// returned no exact match.
	ErrNotFound = errors.New("harbor: not found")

	// ErrInaccessible means Harbor returned 403. Harbor hides existence, so
	// this covers both "deleted" and "no permission" and the two cannot be
	// distinguished from the response alone.
	ErrInaccessible = errors.New("harbor: not found or not permitted")

	// ErrAlreadyExists means a project with the same name already exists.
	ErrAlreadyExists = errors.New("harbor: already exists")

	// ErrNotEmpty means the project still contains repositories.
	ErrNotEmpty = errors.New("harbor: project contains repositories")

	// ErrInvalidName means Harbor rejected the project name. Retrying will
	// not help.
	ErrInvalidName = errors.New("harbor: invalid project name")
)

// APIError carries the Harbor response detail alongside a sentinel that
// callers match with errors.Is.
type APIError struct {
	StatusCode int
	Code       string
	Message    string

	sentinel error
}

func (e *APIError) Error() string {
	return fmt.Sprintf("harbor: %s (%d %s)", e.Message, e.StatusCode, e.Code)
}

func (e *APIError) Unwrap() error { return e.sentinel }

// classify maps an HTTP status and Harbor error code to a sentinel.
//
// Note: 403 is deliberately not mapped to ErrNotFound. Harbor returns 403 for
// both missing and forbidden projects, so callers must fall back to a name
// lookup to tell them apart.
func classify(status int, code, message string) error {
	e := &APIError{StatusCode: status, Code: code, Message: message}

	switch {
	case status == 409:
		e.sentinel = ErrAlreadyExists
	case status == 412 && code == "PRECONDITION":
		e.sentinel = ErrNotEmpty
	case status == 403:
		e.sentinel = ErrInaccessible
	case status == 404:
		e.sentinel = ErrNotFound
	case status == 400:
		e.sentinel = ErrInvalidName
	}
	return e
}
