package harbor

import "context"

// Project is the subset of a Harbor project this operator manages.
type Project struct {
	ID        int64
	Name      string
	Public    bool
	RepoCount int64
}

type Client interface {
	// GetVersion returns the Harbor version, which Harbor discloses only to
	// an authenticated caller. It returns ErrUnauthenticated when Harbor
	// answers without one, so a nil error also proves the credentials work.
	GetVersion(ctx context.Context) (string, error)

	// GetProjectByID returns ErrInaccessible when Harbor answers 403, which
	// covers both a deleted project and a permission problem.
	GetProjectByID(ctx context.Context, id int64) (*Project, error)

	// GetProjectByName returns ErrNotFound when no project matches name
	// exactly. Harbor's name filter is a substring match, so the result is
	// filtered client-side. The list only holds what the credentials may see,
	// so ErrNotFound here does not prove that the project is absent.
	GetProjectByName(ctx context.Context, name string) (*Project, error)

	// CreateProject returns the ID of the new project. Harbor does not
	// include it in the response body, so the project is looked up by name
	// after creation. When that lookup fails the error matches
	// ErrIDUnresolved: the project exists all the same.
	CreateProject(ctx context.Context, name string, public bool) (int64, error)

	UpdateProject(ctx context.Context, id int64, public bool) error

	// DeleteProject returns ErrNotEmpty when the project still holds
	// repositories.
	DeleteProject(ctx context.Context, id int64) error
}
