package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
)

// The operations of the routes that accept an Idempotency-Key. A key counts for one of them only.
const (
	opCreateCategory = "POST /api/admin/categories"
	opCreateModel    = "POST /api/admin/models"
	opCreateUnits    = "POST /api/admin/models/:id/units"
)

func registerAdminAssetRoutes(app *fiber.App, deps Deps) {
	admin := RequireAdmin()

	app.Get("/api/admin/categories", admin, listAdminCategories(deps))
	app.Post("/api/admin/categories", admin, createCategory(deps))

	app.Get("/api/admin/models", admin, listAdminModels(deps))
	app.Post("/api/admin/models", admin, createModel(deps))
	app.Patch("/api/admin/models/:id", admin, updateModel(deps))
	app.Delete("/api/admin/models/:id", admin, deleteModel(deps))
	app.Post("/api/admin/models/:id/units", admin, createUnits(deps))

	app.Get("/api/admin/units", admin, listUnits(deps))
	app.Get("/api/admin/units/:tag", admin, getUnit(deps))
	app.Patch("/api/admin/units/:tag", admin, updateUnit(deps))
	app.Delete("/api/admin/units/:tag", admin, deleteUnit(deps))
	app.Post("/api/admin/units/:tag/lifecycle", admin, changeUnitLifecycle(deps))
	app.Get("/api/admin/units/:tag/events", admin, listUnitEvents(deps))
	app.Get("/api/admin/unit-events", admin, listAllUnitEvents(deps))
}

// listAdminCategories is the route listAdminCategories.
func listAdminCategories(deps Deps) fiber.Handler {
	return listCategories(deps)
}

type categoryForm struct {
	Name string `json:"name"`
	Tone string `json:"tone"`
}

func (f categoryForm) input() catalog.CategoryInput {
	return catalog.CategoryInput{Name: f.Name, Tone: f.Tone}
}

// createCategory is the route createCategory.
func createCategory(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form categoryForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		return createWithKey(c, deps, opCreateCategory, "add the category", func(ctx context.Context, rec catalog.Recorder) (any, error) {
			return deps.Catalog.CreateCategory(ctx, form.input(), rec)
		})
	}
}

// listAdminModels is the route listAdminModels.
func listAdminModels(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		filter, filterFails := modelFilter(c)

		if err := failIfAny(append(fails, filterFails...)...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListModels(ctx, filter, req)
		if err != nil {
			return catalogError("list the models", err)
		}

		return c.JSON(page)
	}
}

type modelForm struct {
	Name        string        `json:"name"`
	CategoryID  *string       `json:"categoryId"`
	NewCategory *categoryForm `json:"newCategory"`
	Description *string       `json:"description"`
	Prefix      string        `json:"prefix"`
}

// createModel is the route createModel.
func createModel(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form modelForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		in := catalog.NewModel{Name: form.Name, CategoryID: form.CategoryID, Description: form.Description, Prefix: form.Prefix}

		if form.NewCategory != nil {
			category := form.NewCategory.input()
			in.NewCategory = &category
		}

		return createWithKey(c, deps, opCreateModel, "add the model", func(ctx context.Context, rec catalog.Recorder) (any, error) {
			return deps.Catalog.CreateModel(ctx, in, rec)
		})
	}
}

type modelPatchForm struct {
	Name        *string `json:"name"`
	CategoryID  *string `json:"categoryId"`
	Description *string `json:"description"`
	Retired     *bool   `json:"retired"`
}

// updateModel is the route updateModel.
func updateModel(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form modelPatchForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		model, err := deps.Catalog.UpdateModel(ctx, c.Params("id"), catalog.ModelPatch(form))
		if err != nil {
			return catalogError("change the model", err)
		}

		return c.JSON(model)
	}
}

// deleteModel is the route deleteModel.
func deleteModel(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		if err := deps.Catalog.DeleteModel(ctx, c.Params("id")); err != nil {
			return catalogError("delete the model", err)
		}

		return c.SendStatus(http.StatusNoContent)
	}
}

type unitForm struct {
	Count     *int    `json:"count"`
	Serial    *string `json:"serial"`
	Condition string  `json:"condition"`
	Place     string  `json:"place"`
}

// createUnits is the route createUnits.
func createUnits(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form unitForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		in := catalog.UnitInput{Count: form.Count, Serial: form.Serial, Condition: form.Condition, Place: form.Place}
		modelID := c.Params("id")

		return createWithKey(c, deps, opCreateUnits, "add the units", func(ctx context.Context, rec catalog.Recorder) (any, error) {
			return deps.Catalog.CreateUnits(ctx, modelID, adminID(c), in, rec)
		})
	}
}

// listUnits is the route listUnits.
func listUnits(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		filter := catalog.UnitFilter{Query: c.Query("q"), ModelID: c.Query("modelId"), CategoryID: c.Query("categoryId"), Status: c.Query("status")}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListUnits(ctx, filter, req)
		if err != nil {
			return catalogError("list the units", err)
		}

		return c.JSON(page)
	}
}

// getUnit is the route getUnit.
func getUnit(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		unit, err := deps.Catalog.GetUnit(ctx, c.Params("tag"))
		if err != nil {
			return catalogError("read the unit", err)
		}

		return c.JSON(unit)
	}
}

// nullableText is a JSON value that tells apart a missing field from a null. A body that names the serial number with null or with an
// empty text clears it, and a body that leaves it out keeps it.
type nullableText struct {
	set   bool
	value *string
}

func (n *nullableText) UnmarshalJSON(data []byte) error {
	n.set = true

	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		n.value = nil

		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return err //nolint:wrapcheck // the caller answers bad_request and never shows the text
	}

	n.value = &text

	return nil
}

type unitPatchForm struct {
	Serial    nullableText `json:"serial"`
	Condition *string      `json:"condition"`
	Place     *string      `json:"place"`
}

// updateUnit is the route updateUnit.
func updateUnit(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form unitPatchForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		in := catalog.UnitPatch{SerialSet: form.Serial.set, Serial: form.Serial.value, Condition: form.Condition, Place: form.Place}

		unit, err := deps.Catalog.UpdateUnit(ctx, c.Params("tag"), adminID(c), in)
		if err != nil {
			return catalogError("change the unit", err)
		}

		return c.JSON(unit)
	}
}

// deleteUnit is the route deleteUnit.
func deleteUnit(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		if err := deps.Catalog.DeleteUnit(ctx, c.Params("tag")); err != nil {
			return catalogError("delete the unit", err)
		}

		return c.SendStatus(http.StatusNoContent)
	}
}

type lifecycleForm struct {
	Lifecycle string  `json:"lifecycle"`
	Note      *string `json:"note"`
}

// changeUnitLifecycle is the route changeUnitLifecycle.
func changeUnitLifecycle(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var form lifecycleForm
		if err := decodeBody(c, &form); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		unit, err := deps.Catalog.ChangeLifecycle(ctx, c.Params("tag"), adminID(c), catalog.LifecycleChange(form))
		if err != nil {
			return catalogError("change the lifecycle", err)
		}

		return c.JSON(unit)
	}
}

// listUnitEvents is the route listUnitEvents.
func listUnitEvents(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListUnitEvents(ctx, c.Params("tag"), req)
		if err != nil {
			return catalogError("list the events of the unit", err)
		}

		return c.JSON(page)
	}
}

// listAllUnitEvents is the route listAllUnitEvents.
func listAllUnitEvents(deps Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		req, fails := readPage(c)
		if err := failIfAny(fails...); err != nil {
			return err
		}

		ctx, cancel := context.WithTimeout(c.Context(), dbTimeout)
		defer cancel()

		page, err := deps.Catalog.ListAllUnitEvents(ctx, req)
		if err != nil {
			return catalogError("list the events of the units", err)
		}

		return c.JSON(page)
	}
}
