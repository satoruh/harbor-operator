// Package fake provides an in-memory harbor.Client for controller tests.
//
// It reproduces the Harbor behaviours the reconciler has to cope with: an
// unknown ID is inaccessible rather than not found, name lookup is exact, and
// a project holding repositories refuses deletion.
package fake

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/satoruh/harbor-operator/internal/harbor"
)

// Client is an in-memory harbor.Client.
//
// The zero value is not usable; call New.
type Client struct {
	mu       sync.Mutex
	projects map[int64]harbor.Project
	nextID   int64

	// Errs forces a method to fail. The key is the method name, for example
	// "CreateProject". The error is returned on every call until removed, and
	// the call has no effect on the stored projects.
	Errs map[string]error

	// Calls records method names in the order they were invoked.
	Calls []string
}

var _ harbor.Client = (*Client)(nil)

// New returns a Client seeded with the given projects. Projects with a zero ID
// are assigned one.
func New(projects ...harbor.Project) *Client {
	c := &Client{
		projects: make(map[int64]harbor.Project, len(projects)),
		nextID:   1,
		Errs:     make(map[string]error),
	}
	for _, p := range projects {
		c.Add(p)
	}
	return c
}

// Add stores a project, assigning an ID when it has none, and returns the ID.
func (c *Client) Add(p harbor.Project) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.add(p)
}

func (c *Client) add(p harbor.Project) int64 {
	if p.ID == 0 {
		p.ID = c.nextID
	}
	if p.ID >= c.nextID {
		c.nextID = p.ID + 1
	}
	c.projects[p.ID] = p
	return p.ID
}

// Projects returns a snapshot of the stored projects, keyed by ID.
func (c *Client) Projects() map[int64]harbor.Project {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[int64]harbor.Project, len(c.projects))
	maps.Copy(out, c.projects)
	return out
}

// enter records the call and reports an injected error, if any. The caller
// must hold no lock; enter takes it and leaves it held.
func (c *Client) enter(method string) error {
	c.mu.Lock()
	c.Calls = append(c.Calls, method)
	return c.Errs[method]
}

func (c *Client) GetProjectByID(_ context.Context, id int64) (*harbor.Project, error) {
	err := c.enter("GetProjectByID")
	defer c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	p, ok := c.projects[id]
	if !ok {
		// Harbor answers 403 for an unknown ID; it does not disclose that the
		// project is missing.
		return nil, harbor.ErrInaccessible
	}
	return &p, nil
}

func (c *Client) GetProjectByName(_ context.Context, name string) (*harbor.Project, error) {
	err := c.enter("GetProjectByName")
	defer c.mu.Unlock()
	if err != nil {
		return nil, err
	}

	for _, p := range c.projects {
		if p.Name == name {
			return &p, nil
		}
	}
	return nil, harbor.ErrNotFound
}

func (c *Client) CreateProject(_ context.Context, name string, public bool) (int64, error) {
	err := c.enter("CreateProject")
	defer c.mu.Unlock()
	if err != nil {
		return 0, err
	}

	for _, p := range c.projects {
		if p.Name == name {
			return 0, harbor.ErrAlreadyExists
		}
	}
	return c.add(harbor.Project{Name: name, Public: public}), nil
}

func (c *Client) UpdateProject(_ context.Context, id int64, public bool) error {
	err := c.enter("UpdateProject")
	defer c.mu.Unlock()
	if err != nil {
		return err
	}

	p, ok := c.projects[id]
	if !ok {
		return harbor.ErrInaccessible
	}
	p.Public = public
	c.projects[id] = p
	return nil
}

func (c *Client) DeleteProject(_ context.Context, id int64) error {
	err := c.enter("DeleteProject")
	defer c.mu.Unlock()
	if err != nil {
		return err
	}

	p, ok := c.projects[id]
	if !ok {
		return harbor.ErrInaccessible
	}
	if p.RepoCount > 0 {
		return fmt.Errorf("project %q holds %d repositories: %w", p.Name, p.RepoCount, harbor.ErrNotEmpty)
	}
	delete(c.projects, id)
	return nil
}
