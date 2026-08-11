package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const projectName = "team"

// listItem mirrors the fields of a Harbor project the client reads.
type listItem struct {
	ProjectID int64             `json:"project_id"`
	Name      string            `json:"name"`
	Metadata  map[string]string `json:"metadata"`
	RepoCount int64             `json:"repo_count"`
}

func item(id int64, name string, public bool) listItem {
	return listItem{
		ProjectID: id,
		Name:      name,
		Metadata:  map[string]string{"public": strconv.FormatBool(public)},
	}
}

// newClient starts a stub Harbor and returns a client pointed at it. baseURL is
// taken verbatim from the server, i.e. without the /api/v2.0 suffix.
func newClient(t *testing.T, h http.HandlerFunc) Client {
	t.Helper()

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c, err := New(Options{BaseURL: srv.URL, Username: "robot$op", Password: "secret"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func writeJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// harborErrors builds the error envelope Harbor returns.
func harborErrors(code, message string) map[string]any {
	return map[string]any{
		"errors": []map[string]string{{"code": code, "message": message}},
	}
}

// TestGetProjectByNameRejectsSubstringMatch is the most important test in this
// package: Harbor's name filter is LIKE %name%, so a lax client would bind a CR
// to an unrelated project.
func TestGetProjectByNameRejectsSubstringMatch(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		results []listItem
		wantID  int64
	}{
		{
			name:  "suffix match is not accepted",
			query: "platform",
			results: []listItem{
				item(5, "platform-staging", false),
				item(3, "platform", false),
			},
			wantID: 3,
		},
		{
			name:  "prefix match is not accepted",
			query: "staging",
			results: []listItem{
				item(5, "platform-staging", false),
				item(9, "staging", false),
			},
			wantID: 9,
		},
		{
			name:  "only substring matches means not found",
			query: "staging",
			results: []listItem{
				item(5, "platform-staging", false),
				item(6, "staging-old", false),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				if got := r.URL.Query().Get("name"); got != tc.query {
					t.Errorf("name filter = %q, want %q", got, tc.query)
				}
				w.Header().Set("X-Total-Count", strconv.Itoa(len(tc.results)))
				writeJSON(t, w, http.StatusOK, tc.results)
			})

			p, err := c.GetProjectByName(context.Background(), tc.query)
			if tc.wantID == 0 {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("err = %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetProjectByName: %v", err)
			}
			if p.ID != tc.wantID {
				t.Errorf("ID = %d, want %d", p.ID, tc.wantID)
			}
			if p.Name != tc.query {
				t.Errorf("Name = %q, want %q", p.Name, tc.query)
			}
		})
	}
}

func TestGetProjectByNamePaginates(t *testing.T) {
	// The exact match sits on the second page, behind a full page of
	// substring matches.
	first := make([]listItem, listPageSize)
	for i := range first {
		first[i] = item(int64(100+i), "team-"+strconv.Itoa(i), false)
	}
	second := []listItem{item(7, projectName, true)}
	total := len(first) + len(second)

	var pages []string
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pages = append(pages, q.Get("page"))
		if got := q.Get("page_size"); got != strconv.Itoa(listPageSize) {
			t.Errorf("page_size = %q, want %d", got, listPageSize)
		}

		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		if q.Get("page") == "1" {
			writeJSON(t, w, http.StatusOK, first)
			return
		}
		writeJSON(t, w, http.StatusOK, second)
	})

	p, err := c.GetProjectByName(context.Background(), projectName)
	if err != nil {
		t.Fatalf("GetProjectByName: %v", err)
	}
	if p.ID != 7 {
		t.Errorf("ID = %d, want 7", p.ID)
	}
	if !p.Public {
		t.Error("Public = false, want true")
	}
	if len(pages) != 2 || pages[0] != "1" || pages[1] != "2" {
		t.Errorf("requested pages = %v, want [1 2]", pages)
	}
}

// TestGetProjectByNameStopsAtTotalCount guards against looping forever when a
// full page contains no exact match and no further page exists.
func TestGetProjectByNameStopsAtTotalCount(t *testing.T) {
	page := make([]listItem, listPageSize)
	for i := range page {
		page[i] = item(int64(100+i), "team-"+strconv.Itoa(i), false)
	}

	var requests int
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests > 2 {
			t.Fatalf("client kept paging past the total count")
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(listPageSize))
		writeJSON(t, w, http.StatusOK, page)
	})

	if _, err := c.GetProjectByName(context.Background(), projectName); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if requests != 1 {
		t.Errorf("requests = %d, want 1", requests)
	}
}

func TestGetProjectByID(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2.0/projects/7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if user, pass, ok := r.BasicAuth(); !ok || user != "robot$op" || pass != "secret" {
			t.Errorf("basic auth = (%q, %q, %v)", user, pass, ok)
		}
		writeJSON(t, w, http.StatusOK, listItem{
			ProjectID: 7,
			Name:      projectName,
			Metadata:  map[string]string{"public": "true"},
			RepoCount: 3,
		})
	})

	p, err := c.GetProjectByID(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetProjectByID: %v", err)
	}
	want := Project{ID: 7, Name: projectName, Public: true, RepoCount: 3}
	if *p != want {
		t.Errorf("project = %+v, want %+v", *p, want)
	}
}

// TestGetProjectByIDForbidden pins the behaviour that makes the two-step
// adoption logic necessary: Harbor hides a missing project behind 403.
func TestGetProjectByIDForbidden(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, http.StatusForbidden, harborErrors("FORBIDDEN", "forbidden"))
	})

	_, err := c.GetProjectByID(context.Background(), 999999)
	if !errors.Is(err, ErrInaccessible) {
		t.Fatalf("err = %v, want ErrInaccessible", err)
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("403 must not be reported as ErrNotFound")
	}
}

func TestCreateProjectLooksUpID(t *testing.T) {
	var created map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Errorf("decode request: %v", err)
			}
			// Harbor answers with an empty body and the ID in Location only.
			w.Header().Set("Location", "/api/v2.0/projects/8")
			w.WriteHeader(http.StatusCreated)
			return
		}
		w.Header().Set("X-Total-Count", "1")
		writeJSON(t, w, http.StatusOK, []listItem{item(8, projectName, false)})
	})

	id, err := c.CreateProject(context.Background(), projectName, false)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if id != 8 {
		t.Errorf("id = %d, want 8", id)
	}
	if got := created["project_name"]; got != projectName {
		t.Errorf("project_name = %v, want team", got)
	}
	// metadata.public is a string in Harbor's API, not a bool.
	meta, _ := created["metadata"].(map[string]any)
	if got := meta["public"]; got != "false" {
		t.Errorf("metadata.public = %#v, want \"false\"", got)
	}
}

func TestUpdateProject(t *testing.T) {
	var body map[string]any
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/api/v2.0/projects/7" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	})

	if err := c.UpdateProject(context.Background(), 7, true); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	meta, _ := body["metadata"].(map[string]any)
	if got := meta["public"]; got != "true" {
		t.Errorf("metadata.public = %#v, want \"true\"", got)
	}
}

func TestDeleteProject(t *testing.T) {
	c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		// Harbor answers 200, not 204.
		w.WriteHeader(http.StatusOK)
	})

	if err := c.DeleteProject(context.Background(), 7); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
}

// TestDeleteProjectNotEmpty covers both messages observed for the same
// condition; the classification must rest on errors[].code alone.
func TestDeleteProjectNotEmpty(t *testing.T) {
	messages := []string{
		"projects {team} contains repositories, can not be deleted",
		"precondition failed: the project contains repositories, can not be deleted",
	}

	for _, message := range messages {
		t.Run(message, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, http.StatusPreconditionFailed, harborErrors("PRECONDITION", message))
			})

			err := c.DeleteProject(context.Background(), 7)
			if !errors.Is(err, ErrNotEmpty) {
				t.Fatalf("err = %v, want ErrNotEmpty", err)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err is not an *APIError: %v", err)
			}
			if apiErr.Message != message {
				t.Errorf("Message = %q, want %q", apiErr.Message, message)
			}
		})
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		name string
		// status and code are what the stub Harbor answers with.
		status int
		code   string
		// wantCode is the code carried by the APIError. It is empty when the
		// swagger spec does not declare the status for this operation: the
		// generated client then reports a bare runtime.APIError with no
		// payload, and only the status survives.
		wantCode   string
		want       error
		noSentinel bool
	}{
		{name: "conflict", status: http.StatusConflict, code: "CONFLICT", wantCode: "CONFLICT", want: ErrAlreadyExists},
		{name: "bad name", status: http.StatusBadRequest, code: "BAD_REQUEST", wantCode: "BAD_REQUEST", want: ErrInvalidName},
		{name: "forbidden", status: http.StatusForbidden, code: "FORBIDDEN", want: ErrInaccessible},
		{name: "server error", status: http.StatusInternalServerError, code: "INTERNAL_SERVER_ERROR", wantCode: "INTERNAL_SERVER_ERROR", noSentinel: true},
		{name: "unauthorized", status: http.StatusUnauthorized, code: "UNAUTHORIZED", wantCode: "UNAUTHORIZED", noSentinel: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(t, w, tc.status, harborErrors(tc.code, "boom"))
			})

			_, err := c.CreateProject(context.Background(), projectName, false)
			if err == nil {
				t.Fatal("CreateProject succeeded, want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("err is not an *APIError: %v", err)
			}
			if apiErr.StatusCode != tc.status {
				t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, tc.status)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if tc.noSentinel {
				// No sentinel: the reconciler should retry rather than branch.
				for _, sentinel := range []error{ErrNotFound, ErrInaccessible, ErrAlreadyExists, ErrNotEmpty, ErrInvalidName} {
					if errors.Is(err, sentinel) {
						t.Errorf("err matched %v, want no sentinel", sentinel)
					}
				}
			}
		})
	}
}

func TestNewBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		suffix  string
		wantErr bool
	}{
		{name: "plain"},
		{name: "trailing slash", suffix: "/"},
		{name: "sub path", suffix: "/harbor"},
		{name: "sub path with trailing slash", suffix: "/harbor/"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("X-Total-Count", "0")
				writeJSON(t, w, http.StatusOK, []listItem{})
			}))
			t.Cleanup(srv.Close)

			c, err := New(Options{BaseURL: srv.URL + tc.suffix})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := c.GetProjectByName(context.Background(), projectName); !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}

			base := tc.suffix
			if base == "/" {
				base = ""
			}
			want := strings.TrimSuffix(base, "/") + "/api/v2.0/projects"
			if gotPath != want {
				t.Errorf("request path = %q, want %q", gotPath, want)
			}
		})
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, url := range []string{"", "harbor.example.com", "://nope"} {
		if _, err := New(Options{BaseURL: url}); err == nil {
			t.Errorf("New(%q) succeeded, want error", url)
		}
	}
}

func TestNewRejectsBadCABundle(t *testing.T) {
	_, err := New(Options{BaseURL: "https://harbor.example.com", CABundle: []byte("not a certificate")})
	if err == nil {
		t.Fatal("New succeeded, want error")
	}
}
