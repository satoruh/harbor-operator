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
	// GetProjectByID returns ErrInaccessible when Harbor answers 403, which
	// covers both a deleted project and a permission problem.
	GetProjectByID(ctx context.Context, id int64) (*Project, error)

	// GetProjectByName returns ErrNotFound when no project matches name
	// exactly. Harbor's name filter is a substring match, so the result is
	// filtered client-side.
	GetProjectByName(ctx context.Context, name string) (*Project, error)

	// CreateProject returns the ID of the new project. Harbor does not
	// include it in the response body, so the project is looked up by name
	// after creation.
	CreateProject(ctx context.Context, name string, public bool) (int64, error)

	UpdateProject(ctx context.Context, id int64, public bool) error

	// DeleteProject returns ErrNotEmpty when the project still holds
	// repositories.
	DeleteProject(ctx context.Context, id int64) error
}
