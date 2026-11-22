package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
)

const idemValue = "c0a80101-example-key"

func idemHeaders() map[string]string {
	return map[string]string{"Idempotency-Key": idemValue}
}

// createRoutes are the three routes of this group that accept an Idempotency-Key, with a body that each one accepts.
var createRoutes = []struct {
	name, path, body, operation string
}{
	{"a category", "/api/admin/categories", `{"name":"Example category","tone":"blue"}`, "POST /api/admin/categories"},
	{"a model", "/api/admin/models", `{"name":"Example headset","categoryId":"` + "00000000-0000-4000-8000-000000000001" + `","prefix":"EXM"}`, "POST /api/admin/models"},
	{"units", "/api/admin/models/" + sampleModelID + "/units", `{"count":1,"condition":"Good","place":"Example shelf"}`, "POST /api/admin/models/:id/units"},
}

func TestTheKeyReachesTheClaimOfItsSessionAndRoute(t *testing.T) {
	for _, route := range createRoutes {
		e := newAssetEnv(t)

		res, body := e.call(e.admin, http.MethodPost, route.path, route.body, idemHeaders())
		if res.StatusCode != http.StatusCreated {
			t.Fatalf("%s answers %d, %s", route.name, res.StatusCode, body)
		}

		if len(e.keys.keys) != 1 {
			t.Fatalf("%s made %d claims", route.name, len(e.keys.keys))
		}

		key := e.keys.keys[0]
		if string(key.Session) != string(e.adminS.TokenHash) || key.Operation != route.operation || key.Value != idemValue {
			t.Fatalf("%s claimed %+v", route.name, key)
		}

		// the write stored its answer through the claim, and the claim stayed
		if e.cat.rec == nil || len(e.keys.claim.recorded) != 1 || e.keys.claim.recorded[0] != 201 || e.keys.claim.released != 0 {
			t.Fatalf("%s recorded %v and released %d", route.name, e.keys.claim.recorded, e.keys.claim.released)
		}
	}
}

func TestARequestWithoutAKeyClaimsNothing(t *testing.T) {
	for _, route := range createRoutes {
		e := newAssetEnv(t)

		res, body := e.call(e.admin, http.MethodPost, route.path, route.body, nil)
		if res.StatusCode != http.StatusCreated || len(e.keys.keys) != 0 || e.cat.rec != nil {
			t.Fatalf("%s answers %d, %s, with %d claims", route.name, res.StatusCode, body, len(e.keys.keys))
		}
	}
}

func TestARepeatGetsTheStoredAnswerAndTheServiceDoesNotRun(t *testing.T) {
	for _, route := range createRoutes {
		e := newAssetEnv(t)
		e.keys.answer = &idempotency.Answer{Status: http.StatusCreated, Body: []byte(`{"id":"stored"}`)}

		// the body of the repeat differs from the first one, and it is not even valid, so the answer shows that it is never read
		res, body := e.call(e.admin, http.MethodPost, route.path, `{"name":"Another"}`, idemHeaders())
		if res.StatusCode != http.StatusCreated || string(body) != `{"id":"stored"}` || !strings.HasPrefix(res.Header.Get("Content-Type"), "application/json") {
			t.Fatalf("%s answers %d, %s", route.name, res.StatusCode, body)
		}

		if len(e.cat.called()) != 0 {
			t.Fatalf("%s called the catalog %v", route.name, e.cat.called())
		}
	}
}

func TestARepeatWhileTheFirstRunsAnswers409(t *testing.T) {
	for _, route := range createRoutes {
		e := newAssetEnv(t)
		e.keys.err = idempotency.ErrInProgress

		res, body := e.call(e.admin, http.MethodPost, route.path, route.body, idemHeaders())
		if res.StatusCode != http.StatusConflict || decodeError(t, body).Error.Code != "request_in_progress" || len(e.cat.called()) != 0 {
			t.Fatalf("%s answers %d, %s", route.name, res.StatusCode, body)
		}
	}
}

func TestAFailedWriteGivesTheKeyBack(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"a clash", &catalog.DuplicateError{Field: "name"}, 409, "duplicate"},
		{"a field", validation("name", "too_short"), 422, "validation_failed"},
		{"a bug", errors.New("example failure"), 500, "internal"},
		{"an outage", &outageError{}, 503, "unavailable"},
		{"a claim that was taken over", fmt.Errorf("record the answer %w", idempotency.ErrLost), 409, "request_in_progress"},
	}

	for _, tc := range tests {
		e := newAssetEnv(t)
		e.cat.err = tc.err

		res, body := e.call(e.admin, http.MethodPost, "/api/admin/categories", createRoutes[0].body, idemHeaders())
		if res.StatusCode != tc.status || decodeError(t, body).Error.Code != tc.code {
			t.Fatalf("%s answers %d, %s", tc.name, res.StatusCode, body)
		}

		if len(e.keys.claim.recorded) != 0 || e.keys.claim.released != 1 {
			t.Fatalf("%s recorded %v and released %d", tc.name, e.keys.claim.recorded, e.keys.claim.released)
		}
	}
}

func TestAKeyOfTheWrongShapeAnswers422BeforeAnyWork(t *testing.T) {
	for _, key := range []string{"short", "has space in it", strings.Repeat("a", 65), "semi;colon-key", "café-example-key"} {
		e := newAssetEnv(t)

		res, body := e.call(e.admin, http.MethodPost, "/api/admin/categories", createRoutes[0].body, map[string]string{"Idempotency-Key": key})
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("the key %q answers %d, %s", key, res.StatusCode, body)
		}

		got := decodeError(t, body)
		if got.Error.Code != "validation_failed" || len(got.Error.Details) != 1 || got.Error.Details[0].Field != "Idempotency-Key" || got.Error.Details[0].Code != "invalid_format" {
			t.Fatalf("the key %q gives %s", key, body)
		}

		if len(e.keys.keys) != 0 || len(e.cat.called()) != 0 {
			t.Fatalf("the key %q reached the store %v or the catalog %v", key, e.keys.keys, e.cat.called())
		}
	}
}

func TestAnOutageWhileClaimingTheKeyAnswers503(t *testing.T) {
	e := newAssetEnv(t)
	e.keys.err = &outageError{}

	res, body := e.call(e.admin, http.MethodPost, "/api/admin/models", createRoutes[1].body, idemHeaders())
	if res.StatusCode != http.StatusServiceUnavailable || decodeError(t, body).Error.Code != "unavailable" || len(e.cat.called()) != 0 {
		t.Fatalf("the answer is %d, %s", res.StatusCode, body)
	}
}

func TestEveryWriteNeedsTheCSRFTokenAndTheOrigin(t *testing.T) {
	writes := []struct{ method, path, body string }{
		{"POST", "/api/admin/categories", createRoutes[0].body},
		{"POST", "/api/admin/models", createRoutes[1].body},
		{"PATCH", "/api/admin/models/" + sampleModelID, `{"retired":true}`},
		{"DELETE", "/api/admin/models/" + sampleModelID, ""},
		{"POST", "/api/admin/models/" + sampleModelID + "/units", createRoutes[2].body},
		{"PATCH", "/api/admin/units/" + sampleTag, `{"place":"Another shelf"}`},
		{"DELETE", "/api/admin/units/" + sampleTag, ""},
		{"POST", "/api/admin/units/" + sampleTag + "/lifecycle", `{"lifecycle":"maintenance"}`},
	}

	for _, w := range writes {
		e := newAssetEnv(t)

		noToken := e.admin
		noToken.csrf = ""

		res, body := e.call(noToken, w.method, w.path, w.body, nil)
		if res.StatusCode != http.StatusForbidden || decodeError(t, body).Error.Code != "csrf_failed" {
			t.Fatalf("%s %s without the token answers %d, %s", w.method, w.path, res.StatusCode, body)
		}

		wrong := e.admin
		wrong.csrf = "not the token"

		res, body = e.call(wrong, w.method, w.path, w.body, nil)
		if res.StatusCode != http.StatusForbidden || decodeError(t, body).Error.Code != "csrf_failed" {
			t.Fatalf("%s %s with a wrong token answers %d, %s", w.method, w.path, res.StatusCode, body)
		}

		res, body = e.call(e.admin, w.method, w.path, w.body, map[string]string{"Origin": "https://evil.example.org"})
		if res.StatusCode != http.StatusForbidden || decodeError(t, body).Error.Code != "origin_not_allowed" {
			t.Fatalf("%s %s from another origin answers %d, %s", w.method, w.path, res.StatusCode, body)
		}

		if len(e.cat.called()) != 0 {
			t.Fatalf("%s %s reached the catalog", w.method, w.path)
		}
	}
}

func TestASetupSessionCannotUseTheAdminRoutes(t *testing.T) {
	e := newAssetEnv(t)

	for _, path := range []string{"/api/admin/units", "/api/admin/models", "/api/admin/categories", "/api/admin/unit-events"} {
		res, body := e.call(e.setup, http.MethodGet, path, "", nil)
		if res.StatusCode != http.StatusForbidden || decodeError(t, body).Error.Code != "forbidden" {
			t.Fatalf("%s answers %d, %s", path, res.StatusCode, body)
		}
	}

	if len(e.cat.called()) != 0 {
		t.Fatalf("the catalog was called %v", e.cat.called())
	}
}

func TestDuplicateAnswersNameTheFieldAndTheOtherUnit(t *testing.T) {
	tests := []struct {
		dup         *catalog.DuplicateError
		wantMessage string
		wantTag     string
	}{
		{&catalog.DuplicateError{Field: "name"}, "Choose another name.", ""},
		{&catalog.DuplicateError{Field: "prefix"}, "Choose another prefix.", ""},
		{&catalog.DuplicateError{Field: "serial", ExistingTag: sampleTag}, "The serial number belongs to MGM-EXM-001.", sampleTag},
		{&catalog.DuplicateError{Field: "serial"}, "Choose another serial number.", ""},
	}

	for _, tc := range tests {
		got := duplicateError(tc.dup)
		if got.Code != CodeDuplicate || len(got.Details) != 1 || got.Details[0].Message != tc.wantMessage || got.Details[0].ExistingTag != tc.wantTag ||
			got.Details[0].Field != tc.dup.Field || got.Details[0].Code != "duplicate" {
			t.Fatalf("%+v gives %+v", tc.dup, got)
		}
	}
}
