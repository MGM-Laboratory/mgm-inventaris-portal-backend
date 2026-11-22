package server

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/jackc/pgx/v5"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/pagination"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const (
	sampleModelID = "00000000-0000-4000-8000-000000000002"
	sampleTag     = "MGM-EXM-001"
)

var (
	sampleCategory = catalog.Category{ID: "00000000-0000-4000-8000-000000000001", Name: "Example category", Tone: "blue"}

	sampleModel = catalog.Model{
		CatalogModel: catalog.CatalogModel{
			ID: sampleModelID, Name: "Example headset", CategoryID: sampleCategory.ID, Description: "A headset for an example prototype.",
			Total: 2, Held: 1, Available: 1,
		},
		Prefix: "EXM",
	}

	sampleSerial = "SN-0001"
	sampleUnit   = catalog.Unit{
		Tag: sampleTag, ModelID: sampleModelID, ModelName: "Example headset", CategoryID: sampleCategory.ID, CategoryName: "Example category",
		Serial: &sampleSerial, Condition: "Good", Place: "Example shelf", Lifecycle: "active", Status: "available", CreatedAt: "2026-11-17T03:00:00Z",
	}

	sampleEvent = catalog.UnitEvent{ID: "00000000-0000-4000-8000-000000000003", Tag: sampleTag, At: "2026-11-17T03:00:00Z", Action: "added"}

	sampleAssetModel = catalog.AssetModel{
		ID: sampleModelID, Name: "Example headset", Description: "A headset for an example prototype.", CategoryID: sampleCategory.ID,
		Category: sampleCategory, Available: 1,
	}
)

// fakeCatalog answers every call with a canned result, or with err when it is set. It records the arguments of the last call.
type fakeCatalog struct {
	mu  sync.Mutex
	err error

	calls     []string
	page      catalog.PageRequest
	modelF    catalog.ModelFilter
	unitF     catalog.UnitFilter
	category  catalog.CategoryInput
	newModel  catalog.NewModel
	modelPtch catalog.ModelPatch
	unitIn    catalog.UnitInput
	unitPtch  catalog.UnitPatch
	change    catalog.LifecycleChange
	id, tag   string
	admin     string
	rec       catalog.Recorder
}

func (f *fakeCatalog) enter(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, name)
}

func (f *fakeCatalog) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fakeCatalog) record(ctx context.Context, rec catalog.Recorder, status int, body any) error {
	if rec == nil {
		return nil
	}

	return rec.Record(ctx, nil, status, body)
}

func (f *fakeCatalog) ListCategories(_ context.Context, req catalog.PageRequest) (pagination.Page[catalog.Category], error) {
	f.enter("ListCategories")
	f.page = req

	return pagination.Page[catalog.Category]{Items: []catalog.Category{sampleCategory}, Total: 1}, f.err
}

func (f *fakeCatalog) ListCatalog(_ context.Context, filter catalog.ModelFilter, req catalog.PageRequest) (pagination.Page[catalog.CatalogModel], error) {
	f.enter("ListCatalog")
	f.modelF, f.page = filter, req

	return pagination.Page[catalog.CatalogModel]{Items: []catalog.CatalogModel{sampleModel.CatalogModel}, Total: 1, NextCursor: "next"}, f.err
}

func (f *fakeCatalog) GetPublicAsset(_ context.Context, tag string) (catalog.PublicAsset, error) {
	f.enter("GetPublicAsset")
	f.tag = tag

	return catalog.PublicAsset{Tag: sampleTag, Status: "available", Model: sampleAssetModel}, f.err
}

func (f *fakeCatalog) GetAdminAsset(_ context.Context, tag string) (catalog.AdminAsset, error) {
	f.enter("GetAdminAsset")
	f.tag = tag

	return catalog.AdminAsset{
		Tag: sampleTag, Status: "available", Model: sampleAssetModel, Serial: &sampleSerial, Condition: "Good", Place: "Example shelf",
	}, f.err
}

func (f *fakeCatalog) CreateCategory(ctx context.Context, in catalog.CategoryInput, rec catalog.Recorder) (catalog.Category, error) {
	f.enter("CreateCategory")
	f.category, f.rec = in, rec

	if f.err != nil {
		return catalog.Category{}, f.err
	}

	return sampleCategory, f.record(ctx, rec, 201, sampleCategory)
}

func (f *fakeCatalog) ListModels(_ context.Context, filter catalog.ModelFilter, req catalog.PageRequest) (pagination.Page[catalog.Model], error) {
	f.enter("ListModels")
	f.modelF, f.page = filter, req

	return pagination.Page[catalog.Model]{Items: []catalog.Model{sampleModel}, Total: 1}, f.err
}

func (f *fakeCatalog) CreateModel(ctx context.Context, in catalog.NewModel, rec catalog.Recorder) (catalog.Model, error) {
	f.enter("CreateModel")
	f.newModel, f.rec = in, rec

	if f.err != nil {
		return catalog.Model{}, f.err
	}

	return sampleModel, f.record(ctx, rec, 201, sampleModel)
}

func (f *fakeCatalog) UpdateModel(_ context.Context, id string, in catalog.ModelPatch) (catalog.Model, error) {
	f.enter("UpdateModel")
	f.id, f.modelPtch = id, in

	return sampleModel, f.err
}

func (f *fakeCatalog) DeleteModel(_ context.Context, id string) error {
	f.enter("DeleteModel")
	f.id = id

	return f.err
}

func (f *fakeCatalog) CreateUnits(ctx context.Context, modelID, adminID string, in catalog.UnitInput, rec catalog.Recorder) (catalog.UnitList, error) {
	f.enter("CreateUnits")
	f.id, f.admin, f.unitIn, f.rec = modelID, adminID, in, rec

	if f.err != nil {
		return catalog.UnitList{}, f.err
	}

	list := catalog.UnitList{Items: []catalog.Unit{sampleUnit}}

	return list, f.record(ctx, rec, 201, list)
}

func (f *fakeCatalog) ListUnits(_ context.Context, filter catalog.UnitFilter, req catalog.PageRequest) (pagination.Page[catalog.Unit], error) {
	f.enter("ListUnits")
	f.unitF, f.page = filter, req

	return pagination.Page[catalog.Unit]{Items: []catalog.Unit{sampleUnit}, Total: 1}, f.err
}

func (f *fakeCatalog) GetUnit(_ context.Context, tag string) (catalog.Unit, error) {
	f.enter("GetUnit")
	f.tag = tag

	return sampleUnit, f.err
}

func (f *fakeCatalog) UpdateUnit(_ context.Context, tag, adminID string, in catalog.UnitPatch) (catalog.Unit, error) {
	f.enter("UpdateUnit")
	f.tag, f.admin, f.unitPtch = tag, adminID, in

	return sampleUnit, f.err
}

func (f *fakeCatalog) DeleteUnit(_ context.Context, tag string) error {
	f.enter("DeleteUnit")
	f.tag = tag

	return f.err
}

func (f *fakeCatalog) ChangeLifecycle(_ context.Context, tag, adminID string, in catalog.LifecycleChange) (catalog.Unit, error) {
	f.enter("ChangeLifecycle")
	f.tag, f.admin, f.change = tag, adminID, in

	return sampleUnit, f.err
}

func (f *fakeCatalog) ListUnitEvents(_ context.Context, tag string, req catalog.PageRequest) (pagination.Page[catalog.UnitEvent], error) {
	f.enter("ListUnitEvents")
	f.tag, f.page = tag, req

	return pagination.Page[catalog.UnitEvent]{Items: []catalog.UnitEvent{sampleEvent}, Total: 1}, f.err
}

func (f *fakeCatalog) ListAllUnitEvents(_ context.Context, req catalog.PageRequest) (pagination.Page[catalog.UnitEvent], error) {
	f.enter("ListAllUnitEvents")
	f.page = req

	return pagination.Page[catalog.UnitEvent]{Items: []catalog.UnitEvent{sampleEvent}, Total: 1}, f.err
}

// fakeClaim counts what a route does with the claim of a key.
type fakeClaim struct {
	mu       sync.Mutex
	recorded []int
	released int
}

func (c *fakeClaim) Record(_ context.Context, _ pgx.Tx, status int, _ any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.recorded = append(c.recorded, status)

	return nil
}

func (c *fakeClaim) Release(context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.released++
}

// fakeIdempotency hands out one claim, a stored answer or an error.
type fakeIdempotency struct {
	mu     sync.Mutex
	claim  *fakeClaim
	answer *idempotency.Answer
	err    error
	keys   []idempotency.Key
}

func (f *fakeIdempotency) Begin(_ context.Context, key idempotency.Key) (idempotency.Claim, *idempotency.Answer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.keys = append(f.keys, key)

	if f.err != nil {
		return nil, nil, f.err
	}

	if f.answer != nil {
		return nil, f.answer, nil
	}

	return f.claim, nil, nil
}

// assetEnv is an application with the fake catalog, the fake keys and one session of each kind.
type assetEnv struct {
	t      *testing.T
	app    *fiber.App
	store  *fakeStore
	cat    *fakeCatalog
	keys   *fakeIdempotency
	admin  client
	guest  client
	setup  client
	adminS session.Session
}

func newAssetEnv(t *testing.T) *assetEnv {
	t.Helper()

	e := &assetEnv{t: t, store: newFakeStore(), cat: &fakeCatalog{}, keys: &fakeIdempotency{claim: &fakeClaim{}}}
	e.app = NewAPI(testConfig(), Deps{Sessions: e.store, Limiter: &fakeLimiter{}, Catalog: e.cat, Idempotency: e.keys})

	token, sess := e.store.add(session.KindAdmin, nil)
	e.admin, e.adminS = client{token: token, csrf: sess.CSRFToken}, sess

	token, sess = e.store.add(session.KindGuest, &session.Identity{Name: "Example Borrower", NimNip: "000000000000000", Email: "example@student.ub.ac.id"})
	e.guest = client{token: token, csrf: sess.CSRFToken}

	token, sess = e.store.add(session.KindAdminSetup, nil)
	e.setup = client{token: token, csrf: sess.CSRFToken}

	return e
}

// call sends a request as the client and returns the answer.
func (e *assetEnv) call(c client, method, path, body string, extra map[string]string) (*http.Response, []byte) {
	e.t.Helper()

	return mustSend(e.t, e.app, method, path, body, c.headers(extra))
}
