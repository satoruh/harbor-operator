package harbor

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means Harbor answered 404, or a lookup by name returned no
	// exact match. Only the 404 proves the project absent: the list leaves
	// out what the credentials may not see, and answers the same empty 200
	// to credentials it did not accept at all.
	ErrNotFound = errors.New("harbor: not found")

	// ErrInaccessible means Harbor returned 403. Harbor hides existence, so
	// this covers both "deleted" and "no permission", and no read tells the
	// two apart. Creating the project does: see ErrAlreadyExists.
	ErrInaccessible = errors.New("harbor: not found or not permitted")

	// ErrAlreadyExists means a project with the same name already exists.
	// Names are unique across Harbor whether or not the caller may see the
	// project, which makes this the one proof of existence that does not
	// depend on permissions.
	ErrAlreadyExists = errors.New("harbor: already exists")

	// ErrNotEmpty means the project still contains repositories.
	ErrNotEmpty = errors.New("harbor: project contains repositories")

	// ErrUnauthenticated means Harbor did not accept the credentials. Harbor
	// does not say so with a status: it answers 200 and treats the caller as
	// anonymous.
	ErrUnauthenticated = errors.New("harbor: credentials not accepted")

	// ErrIDUnresolved means Harbor created the project but the lookup for its
	// ID failed. The project exists; the error that stopped the lookup is
	// wrapped alongside.
	ErrIDUnresolved = errors.New("harbor: project created but its ID could not be looked up")

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
// both missing and forbidden projects.
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
