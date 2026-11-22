package catalog_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/catalog"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/idempotency"
)

func idemKey(operation string) idempotency.Key {
	return idempotency.Key{Session: []byte{7}, Operation: operation, Value: "example-key-one"}
}

func TestAStoredAnswerMeansTheWriteCommitted(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	keys := idempotency.NewStore(e.pool, func() time.Time { return e.now })
	key := idemKey("POST /api/admin/models")

	claim, _, err := keys.Begin(ctx, key)
	if err != nil || claim == nil {
		t.Fatalf("begin gives %v, %v", claim, err)
	}

	model, err := e.store.CreateModel(ctx, catalog.NewModel{
		Name: "Example headset", NewCategory: &catalog.CategoryInput{Name: "Example category", Tone: "blue"}, Prefix: "EXM",
	}, claim)
	if err != nil {
		t.Fatalf("add the model failed %v", err)
	}

	// a repeat gets the stored answer, which is the body that the first request answered
	_, answer, err := keys.Begin(ctx, key)
	if err != nil || answer == nil || answer.Status != 201 {
		t.Fatalf("a repeat gives %v, %v", answer, err)
	}

	var stored catalog.Model
	if err := json.Unmarshal(answer.Body, &stored); err != nil || stored.ID != model.ID || stored.Prefix != "EXM" || stored.RetiredAt != nil {
		t.Fatalf("the stored body is %s, %v", answer.Body, err)
	}

	if n := e.count(`select count(*)::int from models`); n != 1 {
		t.Fatalf("%d models", n)
	}
}

func TestAFailedWriteStoresNoAnswerAndTheKeyCanBeUsedAgain(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	keys := idempotency.NewStore(e.pool, func() time.Time { return e.now })
	key := idemKey("POST /api/admin/models")
	first := e.model("Example headset", "EXM")

	claim, _, err := keys.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	_, err = e.store.CreateModel(ctx, catalog.NewModel{Name: "Example headset", CategoryID: &first.CategoryID, Prefix: "OTH"}, claim)
	if isDuplicate(err, "name") == nil {
		t.Fatalf("the clash gives %v", err)
	}

	if n := e.count(`select count(*)::int from idempotency_keys where state = 'done'`); n != 0 {
		t.Fatalf("%d answers were stored for a failed write", n)
	}

	claim.Release(ctx)

	again, answer, err := keys.Begin(ctx, key)
	if err != nil || again == nil || answer != nil {
		t.Fatalf("the key gives %v, %v, %v", again, answer, err)
	}

	if _, err := e.store.CreateModel(ctx, catalog.NewModel{Name: "Another headset", CategoryID: &first.CategoryID, Prefix: "OTH"}, again); err != nil {
		t.Fatalf("the fixed body failed %v", err)
	}
}

func TestAWriteWhoseClaimWasTakenOverDoesNotCommit(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	keys := idempotency.NewStore(e.pool, func() time.Time { return e.now })
	key := idemKey("POST /api/admin/models/:id/units")
	m := e.model("Example headset", "EXM")

	slow, _, err := keys.Begin(ctx, key)
	if err != nil {
		t.Fatalf("begin failed %v", err)
	}

	e.now = e.now.Add(3 * time.Minute)

	if taken, _, err := keys.Begin(ctx, key); err != nil || taken == nil {
		t.Fatalf("the takeover gives %v, %v", taken, err)
	}

	_, err = e.store.CreateUnits(ctx, m.ID, e.adminID, catalog.UnitInput{Count: ptr(3), Condition: "Good", Place: "Example shelf"}, slow)
	if !errors.Is(err, idempotency.ErrLost) {
		t.Fatalf("the slow write gives %v", err)
	}

	if n := e.count(`select count(*)::int from units`); n != 0 {
		t.Fatalf("%d units were added by a write that lost its claim", n)
	}

	if next := e.count(`select next_unit_number from models where id = $1::uuid`, m.ID); next != 1 {
		t.Fatalf("the counter is %d", next)
	}
}

func TestTwoParallelFirstCallsAddTheUnitsOnce(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	keys := idempotency.NewStore(e.pool, func() time.Time { return e.now })
	key := idemKey("POST /api/admin/models/:id/units")
	m := e.model("Example headset", "EXM")

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claims  int
		blocked int
		answers int
		failed  []error
	)

	gate := make(chan struct{})

	for range 2 {
		wg.Go(func() {
			<-gate

			claim, answer, err := keys.Begin(ctx, key)

			mu.Lock()
			defer mu.Unlock()

			switch {
			case errors.Is(err, idempotency.ErrInProgress):
				blocked++

				return
			case err != nil:
				failed = append(failed, err)

				return
			case answer != nil:
				answers++

				return
			}

			claims++

			if _, err := e.store.CreateUnits(ctx, m.ID, e.adminID, catalog.UnitInput{Count: ptr(4), Condition: "Good", Place: "Example shelf"}, claim); err != nil {
				failed = append(failed, err)
			}
		})
	}

	close(gate)
	wg.Wait()

	if len(failed) != 0 || claims != 1 || blocked+answers != 1 {
		t.Fatalf("%d claims, %d blocked, %d answers, errors %v", claims, blocked, answers, failed)
	}

	if n := e.count(`select count(*)::int from units`); n != 4 {
		t.Fatalf("%d units", n)
	}

	// a later repeat gets the stored list of the first request
	_, answer, err := keys.Begin(ctx, key)
	if err != nil || answer == nil || answer.Status != 201 {
		t.Fatalf("a repeat gives %v, %v", answer, err)
	}

	var list catalog.UnitList
	if err := json.Unmarshal(answer.Body, &list); err != nil || len(list.Items) != 4 || list.Items[0].Tag != "MGM-EXM-001" {
		t.Fatalf("the stored list is %s, %v", answer.Body, err)
	}
}
