package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
)

// assetCase is one request to a route of the catalog and the answer that the contract names for it. The field op is the key of the
// operation in openapi.yaml, so the test of the contract can tell which status codes have a case.
type assetCase struct {
	op      string
	name    string
	as      string
	method  string
	path    string
	body    string
	arrange func(*assetEnv)
	status  int
	code    string
	check   func(*testing.T, *assetEnv, []byte)
}

func fail(err error) func(*assetEnv) {
	return func(e *assetEnv) { e.cat.err = err }
}

func validation(field, code string) error {
	return &catalog.ValidationError{Details: []catalog.FieldError{{Field: field, Code: code, Message: "Example message."}}}
}

func rule(kind catalog.RuleKind) error {
	return &catalog.RuleError{Kind: kind, Message: "Example rule message."}
}

// common returns the cases that every route has, which are no session, the wrong role, a bug and an outage.
func common(op, method, path, body string, adminOnly bool) []assetCase {
	as := "guest"
	if adminOnly {
		as = "admin"
	}

	cases := []assetCase{
		{op: op, name: "no session", as: "none", method: method, path: path, body: body, status: 401, code: "unauthenticated"},
		{op: op, name: "a bug", as: as, method: method, path: path, body: body, arrange: fail(errors.New("example failure")), status: 500, code: "internal"},
		{op: op, name: "an outage", as: as, method: method, path: path, body: body, arrange: fail(&outageError{}), status: 503, code: "unavailable"},
	}

	if adminOnly {
		cases = append(cases, assetCase{op: op, name: "a guest", as: "guest", method: method, path: path, body: body, status: 403, code: "forbidden"})
	}

	return cases
}

func decode[T any](t *testing.T, body []byte) T {
	t.Helper()

	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("the body is not JSON of the right shape %v, %s", err, body)
	}

	return v
}

func hasCalls(t *testing.T, e *assetEnv, want ...string) {
	t.Helper()

	if got := e.cat.called(); !slices.Equal(got, want) {
		t.Fatalf("the catalog was called with %v, want %v", got, want)
	}
}

func listCheck(wantItems int) func(*testing.T, *assetEnv, []byte) {
	return func(t *testing.T, _ *assetEnv, body []byte) {
		t.Helper()

		page := decode[struct {
			Items []json.RawMessage `json:"items"`
			Total *int              `json:"total"`
		}](t, body)

		if len(page.Items) != wantItems || page.Total == nil {
			t.Fatalf("the page is %s", body)
		}
	}
}

func assetCases() []assetCase {
	const (
		unitPath      = "/api/admin/units/" + sampleTag
		modelPath     = "/api/admin/models/" + sampleModelID
		lifecyclePath = unitPath + "/lifecycle"
	)

	var cases []assetCase

	add := func(more ...assetCase) { cases = append(cases, more...) }

	// the routes that a guest or an admin reads
	add(common("GET /api/categories", "GET", "/api/categories", "", false)...)
	add(
		assetCase{op: "GET /api/categories", name: "a guest reads the first page", as: "guest", method: "GET", path: "/api/categories", status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				listCheck(1)(t, e, body)
				hasCalls(t, e, "ListCategories")

				if e.cat.page.Limit != 50 || e.cat.page.Cursor != "" {
					t.Fatalf("the page request is %+v", e.cat.page)
				}
			}},
		assetCase{op: "GET /api/categories", name: "an admin reads it too", as: "admin", method: "GET", path: "/api/categories?limit=100&cursor=abc", status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) {
				if e.cat.page.Limit != 100 || e.cat.page.Cursor != "abc" {
					t.Fatalf("the page request is %+v", e.cat.page)
				}
			}},
		assetCase{op: "GET /api/categories", name: "a limit of 0", as: "guest", method: "GET", path: "/api/categories?limit=0", status: 422, code: "validation_failed"},
	)

	add(common("GET /api/catalog", "GET", "/api/catalog", "", false)...)
	add(
		assetCase{op: "GET /api/catalog", name: "the filters reach the catalog", as: "guest", method: "GET",
			path: "/api/catalog?q=head&categoryId=" + sampleCategory.ID + "&available=true&limit=5&cursor=abc", status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				page := decode[struct {
					Items []struct {
						Total, Held, Available int
					} `json:"items"`
					NextCursor string `json:"nextCursor"`
				}](t, body)

				if len(page.Items) != 1 || page.Items[0].Available != 1 || page.NextCursor != "next" {
					t.Fatalf("the page is %s", body)
				}

				want := catalog.ModelFilter{Query: "head", CategoryID: sampleCategory.ID, Available: true}
				if e.cat.modelF != want || e.cat.page != (catalog.PageRequest{Limit: 5, Cursor: "abc"}) {
					t.Fatalf("the filter is %+v and the page %+v", e.cat.modelF, e.cat.page)
				}
			}},
		assetCase{op: "GET /api/catalog", name: "the flag of the admin list is read and passed on", as: "guest", method: "GET", path: "/api/catalog?includeRetired=true", status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) {
				// the guest route does not read the flag of the admin list, but it passes it on and the store ignores it
				hasCalls(t, e, "ListCatalog")
			}},
		assetCase{op: "GET /api/catalog", name: "a flag that is no boolean", as: "guest", method: "GET", path: "/api/catalog?available=yes", status: 422, code: "validation_failed",
			check: func(t *testing.T, e *assetEnv, body []byte) {
				hasCalls(t, e)

				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "available" || d[0].Code != "invalid_format" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "GET /api/catalog", name: "a search that is too short", as: "guest", method: "GET", path: "/api/catalog?q=a", arrange: fail(validation("q", "too_short")), status: 422, code: "validation_failed"},
		assetCase{op: "GET /api/catalog", name: "a cursor that the service refuses", as: "guest", method: "GET", path: "/api/catalog?cursor=forged", arrange: fail(pagination.ErrCursor), status: 422, code: "validation_failed",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "cursor" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "GET /api/catalog", name: "a setup session may not read", as: "setup", method: "GET", path: "/api/catalog", status: 403, code: "forbidden"},
	)

	add(
		assetCase{op: "GET /api/assets/{tag}", name: "anyone reads the public page", as: "none", method: "GET", path: "/api/assets/" + sampleTag, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				hasCalls(t, e, "GetPublicAsset")

				keys := decode[map[string]json.RawMessage](t, body)
				if len(keys) != 3 || keys["tag"] == nil || keys["status"] == nil || keys["model"] == nil || e.cat.tag != sampleTag {
					t.Fatalf("the public page is %s", body)
				}
			}},
		assetCase{op: "GET /api/assets/{tag}", name: "a guest gets the public page", as: "guest", method: "GET", path: "/api/assets/" + sampleTag, status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) { hasCalls(t, e, "GetPublicAsset") }},
		assetCase{op: "GET /api/assets/{tag}", name: "a setup session gets the public page", as: "setup", method: "GET", path: "/api/assets/" + sampleTag, status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) { hasCalls(t, e, "GetPublicAsset") }},
		assetCase{op: "GET /api/assets/{tag}", name: "an admin gets the details", as: "admin", method: "GET", path: "/api/assets/" + sampleTag, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				hasCalls(t, e, "GetAdminAsset")

				keys := decode[map[string]json.RawMessage](t, body)
				for _, key := range []string{"serial", "condition", "place", "relatedTicket"} {
					if _, ok := keys[key]; !ok {
						t.Fatalf("the admin page has no %s, %s", key, body)
					}
				}
			}},
		assetCase{op: "GET /api/assets/{tag}", name: "an unknown tag", as: "none", method: "GET", path: "/api/assets/MGM-EXM-099", arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "GET /api/assets/{tag}", name: "a bug", as: "none", method: "GET", path: "/api/assets/" + sampleTag, arrange: fail(errors.New("example failure")), status: 500, code: "internal"},
		assetCase{op: "GET /api/assets/{tag}", name: "an outage", as: "none", method: "GET", path: "/api/assets/" + sampleTag, arrange: fail(&outageError{}), status: 503, code: "unavailable"},
		assetCase{op: "GET /api/assets/{tag}", name: "an outage while the session loads", as: "admin", method: "GET", path: "/api/assets/" + sampleTag, status: 503, code: "unavailable",
			arrange: func(e *assetEnv) { e.store.loadErr = &outageError{} }},
	)

	// the routes of the admin for categories
	add(common("GET /api/admin/categories", "GET", "/api/admin/categories", "", true)...)
	add(
		assetCase{op: "GET /api/admin/categories", name: "an admin reads", as: "admin", method: "GET", path: "/api/admin/categories", status: 200, check: listCheck(1)},
		assetCase{op: "GET /api/admin/categories", name: "a limit of 101", as: "admin", method: "GET", path: "/api/admin/categories?limit=101", status: 422, code: "validation_failed",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "limit" || d[0].Code != "out_of_range" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "GET /api/admin/categories", name: "a limit that is no number", as: "admin", method: "GET", path: "/api/admin/categories?limit=many", status: 422, code: "validation_failed",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Code != "invalid_format" {
					t.Fatalf("the details are %+v", d)
				}
			}},
	)

	const categoryBody = `{"name":"Example category","tone":"blue"}`

	add(common("POST /api/admin/categories", "POST", "/api/admin/categories", categoryBody, true)...)
	add(
		assetCase{op: "POST /api/admin/categories", name: "an admin adds", as: "admin", method: "POST", path: "/api/admin/categories", body: categoryBody, status: 201,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				hasCalls(t, e, "CreateCategory")

				if got := decode[catalog.Category](t, body); got != sampleCategory || e.cat.category != (catalog.CategoryInput{Name: "Example category", Tone: "blue"}) || e.cat.rec != nil {
					t.Fatalf("the answer is %s and the input %+v", body, e.cat.category)
				}
			}},
		assetCase{op: "POST /api/admin/categories", name: "a body that is not JSON", as: "admin", method: "POST", path: "/api/admin/categories", body: `{"name":`, status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/categories", name: "a body of the wrong type", as: "admin", method: "POST", path: "/api/admin/categories", body: `{"name":5,"tone":"blue"}`, status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/categories", name: "no body", as: "admin", method: "POST", path: "/api/admin/categories", status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/categories", name: "a name in use", as: "admin", method: "POST", path: "/api/admin/categories", body: categoryBody,
			arrange: fail(&catalog.DuplicateError{Field: "name"}), status: 409, code: "duplicate",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "name" || d[0].Code != "duplicate" || d[0].ExistingTag != "" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "POST /api/admin/categories", name: "a field that fails", as: "admin", method: "POST", path: "/api/admin/categories", body: `{"name":"x","tone":"pink"}`,
			arrange: fail(validation("tone", "invalid_format")), status: 422, code: "validation_failed",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "tone" || d[0].Code != "invalid_format" {
					t.Fatalf("the details are %+v", d)
				}
			}},
	)

	// the routes for models
	add(common("GET /api/admin/models", "GET", "/api/admin/models", "", true)...)
	add(
		assetCase{op: "GET /api/admin/models", name: "an admin reads with the filters", as: "admin", method: "GET",
			path: "/api/admin/models?q=head&categoryId=" + sampleCategory.ID + "&includeRetired=true", status: 200, check: func(t *testing.T, e *assetEnv, body []byte) {
				listCheck(1)(t, e, body)

				if e.cat.modelF != (catalog.ModelFilter{Query: "head", CategoryID: sampleCategory.ID, IncludeRetired: true}) {
					t.Fatalf("the filter is %+v", e.cat.modelF)
				}

				item := decode[struct {
					Items []map[string]json.RawMessage `json:"items"`
				}](t, body).Items[0]
				if string(item["retiredAt"]) != "null" || string(item["prefix"]) != `"EXM"` {
					t.Fatalf("the model is %v", item)
				}
			}},
		assetCase{op: "GET /api/admin/models", name: "a flag that is no boolean", as: "admin", method: "GET", path: "/api/admin/models?includeRetired=1", status: 422, code: "validation_failed"},
	)

	const modelBody = `{"name":"Example headset","categoryId":"00000000-0000-4000-8000-000000000001","description":"A headset.","prefix":"EXM"}`

	add(common("POST /api/admin/models", "POST", "/api/admin/models", modelBody, true)...)
	add(
		assetCase{op: "POST /api/admin/models", name: "a model in an existing category", as: "admin", method: "POST", path: "/api/admin/models", body: modelBody, status: 201,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				if got := decode[catalog.Model](t, body); got.ID != sampleModelID || got.Prefix != "EXM" {
					t.Fatalf("the answer is %s", body)
				}

				in := e.cat.newModel
				if in.Name != "Example headset" || in.CategoryID == nil || *in.CategoryID != sampleCategory.ID || in.NewCategory != nil ||
					in.Description == nil || *in.Description != "A headset." || in.Prefix != "EXM" {
					t.Fatalf("the input is %+v", in)
				}
			}},
		assetCase{op: "POST /api/admin/models", name: "a model with a new category", as: "admin", method: "POST", path: "/api/admin/models",
			body:   `{"name":"Example headset","newCategory":{"name":"Example category","tone":"blue"},"prefix":"EXM"}`,
			status: 201, check: func(t *testing.T, e *assetEnv, _ []byte) {
				in := e.cat.newModel
				if in.CategoryID != nil || in.NewCategory == nil || *in.NewCategory != (catalog.CategoryInput{Name: "Example category", Tone: "blue"}) || in.Description != nil {
					t.Fatalf("the input is %+v", in)
				}
			}},
		assetCase{op: "POST /api/admin/models", name: "a body that is not JSON", as: "admin", method: "POST", path: "/api/admin/models", body: "[1,", status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/models", name: "a prefix in use", as: "admin", method: "POST", path: "/api/admin/models", body: modelBody,
			arrange: fail(&catalog.DuplicateError{Field: "prefix"}), status: 409, code: "duplicate",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "prefix" {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "POST /api/admin/models", name: "both categories", as: "admin", method: "POST", path: "/api/admin/models", body: modelBody,
			arrange: fail(validation("categoryId", "required")), status: 422, code: "validation_failed"},
	)

	const patchBody = `{"description":"A new text.","retired":true}`

	add(common("PATCH /api/admin/models/{id}", "PATCH", modelPath, patchBody, true)...)
	add(
		assetCase{op: "PATCH /api/admin/models/{id}", name: "an admin changes", as: "admin", method: "PATCH", path: modelPath, body: patchBody, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				patch := e.cat.modelPtch
				if decode[catalog.Model](t, body).ID != sampleModelID || e.cat.id != sampleModelID || patch.Name != nil || patch.CategoryID != nil ||
					patch.Description == nil || *patch.Description != "A new text." || patch.Retired == nil || !*patch.Retired {
					t.Fatalf("the answer is %s and the patch %+v", body, patch)
				}
			}},
		assetCase{op: "PATCH /api/admin/models/{id}", name: "a body that is not JSON", as: "admin", method: "PATCH", path: modelPath, body: "nope", status: 400, code: "bad_request"},
		assetCase{op: "PATCH /api/admin/models/{id}", name: "an unknown model", as: "admin", method: "PATCH", path: modelPath, body: patchBody, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "PATCH /api/admin/models/{id}", name: "a retire that an open ticket stops", as: "admin", method: "PATCH", path: modelPath, body: patchBody,
			arrange: fail(rule(catalog.RuleStatusConflict)), status: 409, code: "status_conflict",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if decodeError(t, body).Error.Message != "Example rule message." {
					t.Fatalf("the message is %s", body)
				}
			}},
		assetCase{op: "PATCH /api/admin/models/{id}", name: "a name in use", as: "admin", method: "PATCH", path: modelPath, body: `{"name":"Taken"}`,
			arrange: fail(&catalog.DuplicateError{Field: "name"}), status: 409, code: "duplicate"},
		assetCase{op: "PATCH /api/admin/models/{id}", name: "an empty patch", as: "admin", method: "PATCH", path: modelPath, body: `{}`,
			arrange: fail(validation("body", "required")), status: 422, code: "validation_failed"},
	)

	add(common("DELETE /api/admin/models/{id}", "DELETE", modelPath, "", true)...)
	add(
		assetCase{op: "DELETE /api/admin/models/{id}", name: "an admin deletes", as: "admin", method: "DELETE", path: modelPath, status: 204,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				hasCalls(t, e, "DeleteModel")

				if len(body) != 0 || e.cat.id != sampleModelID {
					t.Fatalf("the body is %q and the id %q", body, e.cat.id)
				}
			}},
		assetCase{op: "DELETE /api/admin/models/{id}", name: "an unknown model", as: "admin", method: "DELETE", path: modelPath, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "DELETE /api/admin/models/{id}", name: "a model that a ticket used", as: "admin", method: "DELETE", path: modelPath,
			arrange: fail(rule(catalog.RuleStatusConflict)), status: 409, code: "status_conflict"},
	)

	// the routes for units
	const unitBody = `{"count":2,"condition":"Good","place":"Example shelf"}`

	add(common("POST /api/admin/models/{id}/units", "POST", modelPath+"/units", unitBody, true)...)
	add(
		assetCase{op: "POST /api/admin/models/{id}/units", name: "an admin adds units", as: "admin", method: "POST", path: modelPath + "/units", body: unitBody, status: 201,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				list := decode[struct {
					Items []catalog.Unit `json:"items"`
				}](t, body)

				in := e.cat.unitIn
				if len(list.Items) != 1 || list.Items[0].Tag != sampleTag || e.cat.id != sampleModelID || e.cat.admin != e.adminS.Admin.ID ||
					in.Count == nil || *in.Count != 2 || in.Serial != nil || in.Condition != "Good" || in.Place != "Example shelf" {
					t.Fatalf("the answer is %s and the input %+v", body, in)
				}
			}},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "one unit with a serial", as: "admin", method: "POST", path: modelPath + "/units",
			body: `{"count":1,"serial":"SN-0001","condition":"Good","place":"Example shelf"}`, status: 201,
			check: func(t *testing.T, e *assetEnv, _ []byte) {
				if s := e.cat.unitIn.Serial; s == nil || *s != "SN-0001" {
					t.Fatalf("the serial is %v", s)
				}
			}},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "a body that is not JSON", as: "admin", method: "POST", path: modelPath + "/units", body: `{"count":"two"}`, status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "an unknown model", as: "admin", method: "POST", path: modelPath + "/units", body: unitBody, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "a serial in use", as: "admin", method: "POST", path: modelPath + "/units", body: unitBody,
			arrange: fail(&catalog.DuplicateError{Field: "serial", ExistingTag: sampleTag}), status: 409, code: "duplicate",
			check: func(t *testing.T, _ *assetEnv, body []byte) {
				if d := decodeError(t, body).Error.Details; len(d) != 1 || d[0].Field != "serial" || d[0].ExistingTag != sampleTag || !strings.Contains(d[0].Message, sampleTag) {
					t.Fatalf("the details are %+v", d)
				}
			}},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "a model that is retired", as: "admin", method: "POST", path: modelPath + "/units", body: unitBody,
			arrange: fail(rule(catalog.RuleStatusConflict)), status: 409, code: "status_conflict"},
		assetCase{op: "POST /api/admin/models/{id}/units", name: "a count above twenty", as: "admin", method: "POST", path: modelPath + "/units", body: `{"count":21}`,
			arrange: fail(validation("count", "out_of_range")), status: 422, code: "validation_failed"},
	)

	add(common("GET /api/admin/units", "GET", "/api/admin/units", "", true)...)
	add(
		assetCase{op: "GET /api/admin/units", name: "an admin reads with the filters", as: "admin", method: "GET",
			path: "/api/admin/units?q=head&modelId=" + sampleModelID + "&categoryId=" + sampleCategory.ID + "&status=borrowed&limit=3", status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				listCheck(1)(t, e, body)

				want := catalog.UnitFilter{Query: "head", ModelID: sampleModelID, CategoryID: sampleCategory.ID, Status: "borrowed"}
				if e.cat.unitF != want || e.cat.page.Limit != 3 {
					t.Fatalf("the filter is %+v and the page %+v", e.cat.unitF, e.cat.page)
				}
			}},
		assetCase{op: "GET /api/admin/units", name: "a status that does not exist", as: "admin", method: "GET", path: "/api/admin/units?status=reserved",
			arrange: fail(validation("status", "invalid_format")), status: 422, code: "validation_failed"},
		assetCase{op: "GET /api/admin/units", name: "a limit of 0", as: "admin", method: "GET", path: "/api/admin/units?limit=0", status: 422, code: "validation_failed"},
	)

	add(common("GET /api/admin/units/{tag}", "GET", unitPath, "", true)...)
	add(
		assetCase{op: "GET /api/admin/units/{tag}", name: "an admin reads", as: "admin", method: "GET", path: unitPath, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				got := decode[map[string]json.RawMessage](t, body)
				for _, key := range []string{"tag", "modelId", "modelName", "categoryId", "categoryName", "serial", "condition", "place", "lifecycle", "lifecycleNote", "status", "createdAt"} {
					if _, ok := got[key]; !ok {
						t.Fatalf("the unit has no %s, %s", key, body)
					}
				}

				// no open ticket means that the field is absent and not null
				if _, ok := got["openTicketNumber"]; ok || e.cat.tag != sampleTag {
					t.Fatalf("the unit is %s", body)
				}
			}},
		assetCase{op: "GET /api/admin/units/{tag}", name: "an unknown tag", as: "admin", method: "GET", path: unitPath, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
	)

	add(common("PATCH /api/admin/units/{tag}", "PATCH", unitPath, `{"condition":"A small scratch"}`, true)...)
	add(
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "a change without the serial", as: "admin", method: "PATCH", path: unitPath, body: `{"condition":"A small scratch"}`, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				p := e.cat.unitPtch
				if decode[catalog.Unit](t, body).Tag != sampleTag || p.SerialSet || p.Serial != nil || p.Condition == nil || *p.Condition != "A small scratch" || p.Place != nil || e.cat.admin != e.adminS.Admin.ID {
					t.Fatalf("the answer is %s and the patch %+v", body, p)
				}
			}},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "a serial of null clears it", as: "admin", method: "PATCH", path: unitPath, body: `{"serial":null}`, status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) {
				if p := e.cat.unitPtch; !p.SerialSet || p.Serial != nil {
					t.Fatalf("the patch is %+v", p)
				}
			}},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "a new serial", as: "admin", method: "PATCH", path: unitPath, body: `{"serial":"SN-0002","place":"Another shelf"}`, status: 200,
			check: func(t *testing.T, e *assetEnv, _ []byte) {
				if p := e.cat.unitPtch; !p.SerialSet || p.Serial == nil || *p.Serial != "SN-0002" || p.Place == nil || *p.Place != "Another shelf" {
					t.Fatalf("the patch is %+v", p)
				}
			}},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "a serial of the wrong type", as: "admin", method: "PATCH", path: unitPath, body: `{"serial":5}`, status: 400, code: "bad_request"},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "an unknown tag", as: "admin", method: "PATCH", path: unitPath, body: `{"place":"Another shelf"}`, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "a serial in use", as: "admin", method: "PATCH", path: unitPath, body: `{"serial":"SN-0002"}`,
			arrange: fail(&catalog.DuplicateError{Field: "serial", ExistingTag: "MGM-EXM-002"}), status: 409, code: "duplicate"},
		assetCase{op: "PATCH /api/admin/units/{tag}", name: "an empty patch", as: "admin", method: "PATCH", path: unitPath, body: `{}`,
			arrange: fail(validation("body", "required")), status: 422, code: "validation_failed"},
	)

	add(common("DELETE /api/admin/units/{tag}", "DELETE", unitPath, "", true)...)
	add(
		assetCase{op: "DELETE /api/admin/units/{tag}", name: "an admin deletes", as: "admin", method: "DELETE", path: unitPath, status: 204,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				if len(body) != 0 || e.cat.tag != sampleTag {
					t.Fatalf("the body is %q and the tag %q", body, e.cat.tag)
				}
			}},
		assetCase{op: "DELETE /api/admin/units/{tag}", name: "an unknown tag", as: "admin", method: "DELETE", path: unitPath, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "DELETE /api/admin/units/{tag}", name: "a unit that a ticket used", as: "admin", method: "DELETE", path: unitPath,
			arrange: fail(rule(catalog.RuleStatusConflict)), status: 409, code: "status_conflict"},
	)

	const lifecycleBody = `{"lifecycle":"maintenance","note":"The screen is cracked."}`

	add(common("POST /api/admin/units/{tag}/lifecycle", "POST", lifecyclePath, lifecycleBody, true)...)
	add(
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "an admin changes the lifecycle", as: "admin", method: "POST", path: lifecyclePath, body: lifecycleBody, status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				c := e.cat.change
				if decode[catalog.Unit](t, body).Tag != sampleTag || c.Lifecycle != "maintenance" || c.Note == nil || *c.Note != "The screen is cracked." ||
					e.cat.tag != sampleTag || e.cat.admin != e.adminS.Admin.ID {
					t.Fatalf("the answer is %s and the change %+v", body, c)
				}
			}},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "a body that is not JSON", as: "admin", method: "POST", path: lifecyclePath, body: "{", status: 400, code: "bad_request"},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "an unknown tag", as: "admin", method: "POST", path: lifecyclePath, body: lifecycleBody, arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "a step that the table refuses", as: "admin", method: "POST", path: lifecyclePath, body: lifecycleBody,
			arrange: fail(rule(catalog.RuleStatusConflict)), status: 409, code: "status_conflict"},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "a unit in an open ticket", as: "admin", method: "POST", path: lifecyclePath, body: lifecycleBody,
			arrange: fail(rule(catalog.RuleUnitHeld)), status: 409, code: "unit_held"},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "less stock than is held", as: "admin", method: "POST", path: lifecyclePath, body: lifecycleBody,
			arrange: fail(rule(catalog.RuleStockChanged)), status: 409, code: "stock_changed"},
		assetCase{op: "POST /api/admin/units/{tag}/lifecycle", name: "a step to lost without a note", as: "admin", method: "POST", path: lifecyclePath, body: `{"lifecycle":"lost"}`,
			arrange: fail(validation("note", "required")), status: 422, code: "validation_failed"},
	)

	add(common("GET /api/admin/units/{tag}/events", "GET", unitPath+"/events", "", true)...)
	add(
		assetCase{op: "GET /api/admin/units/{tag}/events", name: "an admin reads", as: "admin", method: "GET", path: unitPath + "/events?limit=2", status: 200,
			check: func(t *testing.T, e *assetEnv, body []byte) {
				listCheck(1)(t, e, body)

				got := decode[struct {
					Items []map[string]json.RawMessage `json:"items"`
				}](t, body).Items[0]
				for _, key := range []string{"id", "tag", "at", "action", "adminName", "note", "ticketNumber"} {
					if _, ok := got[key]; !ok {
						t.Fatalf("the event has no %s, %s", key, body)
					}
				}

				if e.cat.tag != sampleTag || e.cat.page.Limit != 2 {
					t.Fatalf("the tag is %q and the page %+v", e.cat.tag, e.cat.page)
				}
			}},
		assetCase{op: "GET /api/admin/units/{tag}/events", name: "an unknown tag", as: "admin", method: "GET", path: unitPath + "/events", arrange: fail(catalog.ErrNotFound), status: 404, code: "not_found"},
		assetCase{op: "GET /api/admin/units/{tag}/events", name: "a limit of 0", as: "admin", method: "GET", path: unitPath + "/events?limit=0", status: 422, code: "validation_failed"},
	)

	add(common("GET /api/admin/unit-events", "GET", "/api/admin/unit-events", "", true)...)
	add(
		assetCase{op: "GET /api/admin/unit-events", name: "an admin reads", as: "admin", method: "GET", path: "/api/admin/unit-events", status: 200, check: listCheck(1)},
		assetCase{op: "GET /api/admin/unit-events", name: "a cursor that the service refuses", as: "admin", method: "GET", path: "/api/admin/unit-events?cursor=forged",
			arrange: fail(pagination.ErrCursor), status: 422, code: "validation_failed"},
		assetCase{op: "GET /api/admin/unit-events", name: "a limit of 0", as: "admin", method: "GET", path: "/api/admin/unit-events?limit=0", status: 422, code: "validation_failed"},
	)

	return cases
}

func (e *assetEnv) clientFor(as string) client {
	switch as {
	case "admin":
		return e.admin
	case "guest":
		return e.guest
	case "setup":
		return e.setup
	default:
		return client{}
	}
}

func TestEveryRouteAnswersEveryStatusOfTheContract(t *testing.T) {
	for _, tc := range assetCases() {
		t.Run(tc.op+" "+tc.name, func(t *testing.T) {
			e := newAssetEnv(t)

			if tc.arrange != nil {
				tc.arrange(e)
			}

			res, body := e.call(e.clientFor(tc.as), tc.method, tc.path, tc.body, nil)
			if res.StatusCode != tc.status {
				t.Fatalf("the status is %d, want %d, %s", res.StatusCode, tc.status, body)
			}

			if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get("X-Request-Id") == "" {
				t.Fatalf("the headers are %v", res.Header)
			}

			if tc.code != "" {
				got := decodeError(t, body)
				if got.Error.Code != tc.code || got.Error.Message == "" || got.Error.RequestID != res.Header.Get("X-Request-Id") {
					t.Fatalf("the error is %s", body)
				}
			} else if tc.status != http.StatusNoContent && !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
				t.Fatalf("the content type is %q", res.Header.Get("Content-Type"))
			}

			if tc.check != nil {
				tc.check(t, e, body)
			}
		})
	}
}

// contractCodes lists the status codes that openapi.yaml names for each operation of this group. A script made the list from the
// contract (the paths and the response codes of the operationIds listed in the task), and it must be made again when the contract changes.
var contractCodes = map[string][]int{
	"GET /api/categories":                   {200, 401, 422, 500, 503},
	"GET /api/catalog":                      {200, 401, 422, 500, 503},
	"GET /api/assets/{tag}":                 {200, 404, 500, 503},
	"GET /api/admin/categories":             {200, 401, 403, 422, 500, 503},
	"POST /api/admin/categories":            {201, 400, 401, 403, 409, 422, 500, 503},
	"GET /api/admin/models":                 {200, 401, 403, 422, 500, 503},
	"POST /api/admin/models":                {201, 400, 401, 403, 409, 422, 500, 503},
	"PATCH /api/admin/models/{id}":          {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"DELETE /api/admin/models/{id}":         {204, 401, 403, 404, 409, 500, 503},
	"POST /api/admin/models/{id}/units":     {201, 400, 401, 403, 404, 409, 422, 500, 503},
	"GET /api/admin/units":                  {200, 401, 403, 422, 500, 503},
	"GET /api/admin/units/{tag}":            {200, 401, 403, 404, 500, 503},
	"PATCH /api/admin/units/{tag}":          {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"DELETE /api/admin/units/{tag}":         {204, 401, 403, 404, 409, 500, 503},
	"POST /api/admin/units/{tag}/lifecycle": {200, 400, 401, 403, 404, 409, 422, 500, 503},
	"GET /api/admin/units/{tag}/events":     {200, 401, 403, 404, 422, 500, 503},
	"GET /api/admin/unit-events":            {200, 401, 403, 422, 500, 503},
}

func TestEveryStatusOfTheContractHasACase(t *testing.T) {
	have := map[string]map[int]bool{}

	for _, tc := range assetCases() {
		if have[tc.op] == nil {
			have[tc.op] = map[int]bool{}
		}

		have[tc.op][tc.status] = true
	}

	for op, codes := range contractCodes {
		for _, status := range codes {
			if !have[op][status] {
				t.Errorf("%s has no case for the status %d", op, status)
			}
		}

		// a case with a status that the contract does not list is a gap of the contract, and the report names each one
		for status := range have[op] {
			if !slices.Contains(codes, status) {
				t.Logf("%s answers %d, which the contract does not list", op, status)
			}
		}
	}

	if len(have) != len(contractCodes) {
		t.Fatalf("the cases cover %d operations and the contract list has %d", len(have), len(contractCodes))
	}
}
