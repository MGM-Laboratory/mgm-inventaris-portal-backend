package server

import (
	"context"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

func registerCatalogRoutes(app *fiber.App, deps Deps) {
	app.Get("/api/categories", RequireReader(), listCategories(deps))
	app.Get("/api/catalog", RequireReader(), listCatalog(deps))
	app.Get("/api/assets/:tag", getAsset(deps))
}

// listCategories is the route listCategories.
func listCategories(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListCategories(ctx, req)
		if err != nil {
			return catalogError("list the categories", err)
		}

		return c.JSON(page)
	}
}

// modelFilter reads the filters that the guest catalog and the admin list of models share.
func modelFilter(c fiber.Ctx) (catalog.ModelFilter, []*Detail) {
	available, availableFail := queryBool(c, "available")
	includeRetired, retiredFail := queryBool(c, "includeRetired")

	return catalog.ModelFilter{
		Query: c.Query("q"), CategoryID: c.Query("categoryId"), Available: available, IncludeRetired: includeRetired,
	}, []*Detail{availableFail, retiredFail}
}

// listCatalog is the route listCatalog.
func listCatalog(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		filter, filterFails := modelFilter(c)

		if err := failIfAny(append(fails, filterFails...)...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListCatalog(ctx, filter, req)
		if err != nil {
			return catalogError("list the catalog", err)
		}

		return c.JSON(page)
	}
}

// getAsset is the route getAsset. Anyone may call it, and only an admin session gets the details of the unit. A session that failed
// to load because of the database answers like any other route, so an admin never gets the page of a stranger by accident.
func getAsset(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		if state := stateOf(c); state.loadErr != nil {
			return dbError("load the session", state.loadErr)
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		if sess := currentSession(c); sess != nil && sess.Kind == session.KindAdmin {
			asset, err := deps.Catalog.GetAdminAsset(ctx, c.Params("tag"))
			if err != nil {
				return catalogError("read the asset", err)
			}

			return c.JSON(asset)
		}

		asset, err := deps.Catalog.GetPublicAsset(ctx, c.Params("tag"))
		if err != nil {
			return catalogError("read the asset", err)
		}

		return c.JSON(asset)
	}
}

// createWithKey runs the write of a route that accepts an Idempotency-Key. Without the header the write runs as it is. With it, the
// first request claims the key and the write stores its answer in its own transaction, and a repeat gets the stored answer, whatever
// its body says. A request that fails gives the key back, so a client may fix the body and send the same key again.
func createWithKey(c fiber.Ctx, deps Deps, operation, what string, run func(ctx context.Context, rec catalog.Recorder) (any, error)) error {
	key := c.Get(idempotencyHeader)

	ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
	defer cancel()

	var rec catalog.Recorder

	if key != "" {
		if !idempotency.ValidKey(key) {
			return NewError(CodeValidationFailed, *detail(idempotencyHeader, detailInvalidFormat, "Use 8 to 64 letters, digits, hyphens or underscores."))
		}

		claim, answer, err := deps.Idempotency.Begin(ctx, idempotency.Key{Session: currentSession(c).TokenHash, Operation: operation, Value: key})
		if err != nil {
			return catalogError("claim the idempotency key", err)
		}

		if answer != nil {
			c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)

			return c.Status(answer.Status).Send(answer.Body)
		}

		rec = claim
	}

	body, err := run(ctx, rec)
	if err != nil {
		if claim, ok := rec.(idempotency.Claim); ok {
			claim.Release(ctx)
		}

		return catalogError(what, err)
	}

	return c.Status(http.StatusCreated).JSON(body)
}

func adminID(c fiber.Ctx) string {
	return currentSession(c).Admin.ID
}
