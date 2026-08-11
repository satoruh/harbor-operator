package harbor

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"
)

// TestIntegration exercises the client against a real Harbor. It is skipped
// unless HARBOR_URL is set:
//
//	HARBOR_URL=https://harbor.example.com \
//	HARBOR_USERNAME='robot$harbor-operator' \
//	HARBOR_PASSWORD=... \
//	go test -count=1 -run TestIntegration ./internal/harbor/
//
// The account must be a system-level robot with Project: Create; an OIDC
// user's CLI secret is rejected by some endpoints. The test creates and
// removes projects named op-test-*.
func TestIntegration(t *testing.T) {
	baseURL := os.Getenv("HARBOR_URL")
	if baseURL == "" {
		t.Skip("HARBOR_URL is not set")
	}

	insecure, _ := strconv.ParseBool(os.Getenv("HARBOR_INSECURE"))
	c, err := New(Options{
		BaseURL:            baseURL,
		Username:           os.Getenv("HARBOR_USERNAME"),
		Password:           os.Getenv("HARBOR_PASSWORD"),
		InsecureSkipVerify: insecure,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	name := "op-test-" + strconv.FormatInt(time.Now().Unix(), 10)
	// The suffixed project exists to prove the name filter is a substring
	// match and that the client narrows it down.
	suffixed := name + "-suffix"

	if _, err := c.GetProjectByName(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("lookup before create: err = %v, want ErrNotFound", err)
	}

	id, err := c.CreateProject(ctx, name, false)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	t.Cleanup(func() {
		if err := deleteIfPresent(c, id); err != nil {
			t.Errorf("cleanup of %s: %v", name, err)
		}
	})

	suffixedID, err := c.CreateProject(ctx, suffixed, false)
	if err != nil {
		t.Fatalf("CreateProject(%s): %v", suffixed, err)
	}
	t.Cleanup(func() {
		if err := deleteIfPresent(c, suffixedID); err != nil {
			t.Errorf("cleanup of %s: %v", suffixed, err)
		}
	})

	p, err := c.GetProjectByName(ctx, name)
	if err != nil {
		t.Fatalf("GetProjectByName: %v", err)
	}
	if p.ID != id || p.Name != name {
		t.Fatalf("lookup returned %+v, want ID %d name %q", *p, id, name)
	}

	if _, err := c.CreateProject(ctx, name, false); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate create: err = %v, want ErrAlreadyExists", err)
	}

	if err := c.UpdateProject(ctx, id, true); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	p, err = c.GetProjectByID(ctx, id)
	if err != nil {
		t.Fatalf("GetProjectByID: %v", err)
	}
	if !p.Public {
		t.Error("Public = false after update, want true")
	}

	if err := c.DeleteProject(ctx, id); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	// Which status a vanished project produces depends on the credentials:
	// an admin sees 404, a robot sees 403 because Harbor hides the existence
	// of projects it will not show. Accept either; the reconciler tells them
	// apart by falling back to a name lookup.
	if _, err := c.GetProjectByID(ctx, id); !errors.Is(err, ErrInaccessible) && !errors.Is(err, ErrNotFound) {
		t.Errorf("lookup after delete: err = %v, want ErrInaccessible or ErrNotFound", err)
	}
	if _, err := c.GetProjectByName(ctx, name); !errors.Is(err, ErrNotFound) {
		t.Errorf("name lookup after delete: err = %v, want ErrNotFound", err)
	}

	// A project name that Harbor rejects outright.
	if _, err := c.CreateProject(ctx, "Op-Test-Invalid", false); !errors.Is(err, ErrInvalidName) {
		t.Errorf("invalid name: err = %v, want ErrInvalidName", err)
	}
}

// deleteIfPresent removes a project, treating an already-gone project as
// success. Harbor reports that as 403 or 404 depending on the credentials.
func deleteIfPresent(c Client, id int64) error {
	err := c.DeleteProject(context.Background(), id)
	if errors.Is(err, ErrInaccessible) || errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
