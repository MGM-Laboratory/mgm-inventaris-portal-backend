package session_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

func ticketID(n int) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", n)
}

func viewableOf(t *testing.T, store *session.Store, token string) []string {
	t.Helper()

	sess, err := store.Load(context.Background(), token)
	if err != nil {
		t.Fatalf("load the session %v", err)
	}

	return sess.ViewableTickets
}

func TestAddViewableTicketKeepsTheOrderAndMovesARepeatToTheEnd(t *testing.T) {
	store, pool, _ := setup(t)
	ctx := context.Background()

	token, sess, err := store.CreateGuest(ctx, nil, nil)
	if err != nil {
		t.Fatalf("make the session %v", err)
	}

	if got := viewableOf(t, store, token); len(got) != 0 {
		t.Fatalf("a new session may read %v", got)
	}

	for _, n := range []int{1, 2, 3, 1} {
		found, err := session.AddViewableTicket(ctx, pool, sess.TokenHash, ticketID(n))
		if err != nil || !found {
			t.Fatalf("add %d gave %v, %v", n, found, err)
		}
	}

	want := []string{ticketID(2), ticketID(3), ticketID(1)}
	if got := viewableOf(t, store, token); !slices.Equal(got, want) {
		t.Fatalf("the list is %v, want %v", got, want)
	}
}

func TestAddViewableTicketForgetsTheOldestBeyondTwenty(t *testing.T) {
	store, _, _ := setup(t)
	ctx := context.Background()

	token, sess, err := store.CreateGuest(ctx, nil, nil)
	if err != nil {
		t.Fatalf("make the session %v", err)
	}

	for n := 1; n <= 25; n++ {
		if err := store.AddViewableTicket(ctx, sess.TokenHash, ticketID(n)); err != nil {
			t.Fatalf("add %d failed %v", n, err)
		}
	}

	got := viewableOf(t, store, token)
	if len(got) != session.MaxViewableTickets || got[0] != ticketID(6) || got[len(got)-1] != ticketID(25) {
		t.Fatalf("the list is %v", got)
	}
}

func TestAddViewableTicketOnAGoneSessionIsNotAnError(t *testing.T) {
	store, pool, _ := setup(t)

	found, err := session.AddViewableTicket(context.Background(), pool, make([]byte, 32), ticketID(1))
	if err != nil || found {
		t.Fatalf("a missing session gave %v, %v", found, err)
	}

	if err := store.AddViewableTicket(context.Background(), make([]byte, 32), ticketID(1)); err != nil {
		t.Fatalf("the store method gave %v", err)
	}
}

func TestParallelAddsOfOneSessionLoseNoTicket(t *testing.T) {
	store, _, _ := setup(t)
	ctx := context.Background()

	token, sess, err := store.CreateGuest(ctx, nil, nil)
	if err != nil {
		t.Fatalf("make the session %v", err)
	}

	const n = 12

	var wg sync.WaitGroup

	gate := make(chan struct{})
	errs := make([]error, n)

	for i := range n {
		wg.Go(func() {
			<-gate

			errs[i] = store.AddViewableTicket(ctx, sess.TokenHash, ticketID(i+1))
		})
	}

	close(gate)
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			t.Fatalf("an add failed %v", err)
		}
	}

	got := viewableOf(t, store, token)
	slices.Sort(got)

	want := make([]string, 0, n)
	for i := range n {
		want = append(want, ticketID(i+1))
	}

	if !slices.Equal(got, want) {
		t.Fatalf("the list is %v, want %v", got, want)
	}
}
