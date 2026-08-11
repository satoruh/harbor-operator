package harbor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-openapi/runtime"
	httptransport "github.com/go-openapi/runtime/client"
	v2client "github.com/goharbor/go-client/pkg/sdk/v2.0/client"
	"github.com/goharbor/go-client/pkg/sdk/v2.0/client/project"
	"github.com/goharbor/go-client/pkg/sdk/v2.0/models"
)

// listPageSize is the page size used when scanning the project list for an
// exact name match. Harbor defaults to 10, which would make the scan page more
// often than necessary.
const listPageSize = 100

// Options describes how to reach a Harbor instance.
type Options struct {
	// BaseURL is the root of the Harbor API without the /api/v2.0 suffix.
	BaseURL string

	Username string
	Password string

	// CABundle is a PEM-encoded set of certificates used to verify the Harbor
	// certificate. Empty means the system pool.
	CABundle []byte

	InsecureSkipVerify bool
}

type client struct {
	projects *project.Client
}

// New builds a Client for a single Harbor instance.
func New(opts Options) (Client, error) {
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("harbor: invalid base URL %q: %w", opts.BaseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("harbor: base URL %q must include a scheme and host", opts.BaseURL)
	}

	// go-client takes the API base path from URL.Path and only substitutes its
	// own default when the path is empty, so a base URL with a trailing slash
	// would silently drop /api/v2.0. Build the full path here instead.
	u.Path = strings.TrimSuffix(u.Path, "/") + v2client.DefaultBasePath

	tr, err := newTransport(opts)
	if err != nil {
		return nil, err
	}

	api := v2client.New(v2client.Config{
		URL:       u,
		Transport: tr,
		AuthInfo:  httptransport.BasicAuth(opts.Username, opts.Password),
	})
	return &client{projects: api.Project}, nil
}

// newTransport always returns an explicit transport: go-client substitutes its
// own InsecureTransport, which skips certificate verification, whenever the
// caller leaves it nil.
func newTransport(opts Options) (http.RoundTripper, error) {
	tlsConf := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: opts.InsecureSkipVerify, // #nosec G402 -- opt-in via spec.insecureSkipVerify
	}
	if len(opts.CABundle) > 0 {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(opts.CABundle) {
			return nil, errors.New("harbor: CA bundle contains no PEM-encoded certificate")
		}
		tlsConf.RootCAs = pool
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = tlsConf
	return tr, nil
}

func (c *client) GetProjectByID(ctx context.Context, id int64) (*Project, error) {
	isName := false
	params := project.NewGetProjectParamsWithContext(ctx).
		WithProjectNameOrID(strconv.FormatInt(id, 10)).
		WithXIsResourceName(&isName)

	ok, err := c.projects.GetProject(ctx, params)
	if err != nil {
		return nil, wrap(err)
	}
	return convert(ok.Payload), nil
}

func (c *client) GetProjectByName(ctx context.Context, name string) (*Project, error) {
	page := int64(1)
	size := int64(listPageSize)

	for {
		params := project.NewListProjectsParamsWithContext(ctx).
			WithName(&name).
			WithPage(&page).
			WithPageSize(&size)

		ok, err := c.projects.ListProjects(ctx, params)
		if err != nil {
			return nil, wrap(err)
		}

		for _, p := range ok.Payload {
			// Harbor filters with LIKE %name%, so "staging" also matches
			// "platform-staging". Only an exact match identifies the project;
			// accepting a substring hit would bind the CR to a stranger.
			if p.Name == name {
				return convert(p), nil
			}
		}

		if int64(len(ok.Payload)) < size || page*size >= ok.XTotalCount {
			return nil, ErrNotFound
		}
		page++
	}
}

func (c *client) CreateProject(ctx context.Context, name string, public bool) (int64, error) {
	params := project.NewCreateProjectParamsWithContext(ctx).
		WithProject(&models.ProjectReq{
			ProjectName: name,
			Metadata:    &models.ProjectMetadata{Public: strconv.FormatBool(public)},
		})

	if _, err := c.projects.CreateProject(ctx, params); err != nil {
		return 0, wrap(err)
	}

	// Harbor answers 201 with an empty body and puts the ID in the Location
	// header only. Looking the project up by name instead of parsing that
	// header keeps this on the same recovery path as a create whose status
	// update failed afterwards.
	p, err := c.GetProjectByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("harbor: created project %q but could not look it up: %w", name, err)
	}
	return p.ID, nil
}

func (c *client) UpdateProject(ctx context.Context, id int64, public bool) error {
	isName := false
	params := project.NewUpdateProjectParamsWithContext(ctx).
		WithProjectNameOrID(strconv.FormatInt(id, 10)).
		WithXIsResourceName(&isName).
		WithProject(&models.ProjectReq{
			Metadata: &models.ProjectMetadata{Public: strconv.FormatBool(public)},
		})

	_, err := c.projects.UpdateProject(ctx, params)
	return wrap(err)
}

func (c *client) DeleteProject(ctx context.Context, id int64) error {
	isName := false
	params := project.NewDeleteProjectParamsWithContext(ctx).
		WithProjectNameOrID(strconv.FormatInt(id, 10)).
		WithXIsResourceName(&isName)

	_, err := c.projects.DeleteProject(ctx, params)
	return wrap(err)
}

func convert(p *models.Project) *Project {
	out := &Project{
		ID:        int64(p.ProjectID),
		Name:      p.Name,
		RepoCount: p.RepoCount,
	}
	if p.Metadata != nil {
		// Harbor encodes the flag as the string "true" / "false".
		out.Public, _ = strconv.ParseBool(p.Metadata.Public)
	}
	return out
}

// statusResponse is implemented by every generated error response. The status
// code is not exposed as a field, only through IsCode.
type statusResponse interface {
	IsCode(int) bool
	GetPayload() *models.Errors
}

// classifiedStatuses are probed against IsCode in order. Any status outside
// this set yields an APIError without a sentinel, which callers treat as a
// generic failure worth retrying.
var classifiedStatuses = []int{400, 401, 403, 404, 409, 412, 500}

// wrap maps a go-client error onto one of the sentinels so that callers can
// use errors.Is without knowing about HTTP.
func wrap(err error) error {
	if err == nil {
		return nil
	}

	var resp statusResponse
	if !errors.As(err, &resp) {
		var apiErr *runtime.APIError
		if errors.As(err, &apiErr) {
			return classify(apiErr.Code, "", apiErr.Error())
		}
		return err
	}

	status := 0
	for _, code := range classifiedStatuses {
		if resp.IsCode(code) {
			status = code
			break
		}
	}

	code, message := "", err.Error()
	if p := resp.GetPayload(); p != nil && len(p.Errors) > 0 {
		code, message = p.Errors[0].Code, p.Errors[0].Message
	}
	return classify(status, code, message)
}
