package session_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

func insertAdmin(t *testing.T, pool *pgxpool.Pool, email string) session.Admin {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var id string
	if err := pool.QueryRow(ctx, `insert into admins (email, name, password_hash) values ($1, 'Example Admin', 'hash') returning id::text`, email).Scan(&id); err != nil {
		t.Fatalf("insert the admin %v", err)
	}

	return session.Admin{ID: id, Name: "Example Admin", Email: email}
}

func countHash(t *testing.T, pool *pgxpool.Pool, hash []byte) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var n int
	if err := pool.QueryRow(ctx, `select count(*) from sessions where token_hash = $1`, hash).Scan(&n); err != nil {
		t.Fatalf("count failed %v", err)
	}

	return n
}

func TestCreateAdminAndLoad(t *testing.T) {
	store, pool, clk := setup(t)
	ctx := context.Background()
	adm := insertAdmin(t, pool, "admin@example.org")

	tests := []struct {
		kind     session.Kind
		absolute time.Duration
		idle     time.Duration
	}{
		{session.KindAdmin, 8 * time.Hour, 30 * time.Minute},
		{session.KindAdminSetup, 15 * time.Minute, 15 * time.Minute},
	}

	for _, tc := range tests {
		token, created, err := store.CreateAdmin(ctx, adm, tc.kind, nil, false)
		if err != nil {
			t.Fatalf("%s create failed %v", tc.kind, err)
		}

		loaded, err := store.Load(ctx, token)
		if err != nil {
			t.Fatalf("%s load failed %v", tc.kind, err)
		}

		if loaded.Kind != tc.kind || loaded.Admin == nil || *loaded.Admin != adm || loaded.Identity != nil {
			t.Fatalf("%s loaded %+v", tc.kind, loaded)
		}

		start := clk.Now()
		if !loaded.ExpiresAt.Equal(start.Add(tc.absolute)) || !loaded.IdleExpiresAt().Equal(start.Add(tc.idle)) || loaded.CSRFToken != created.CSRFToken {
			t.Fatalf("%s has the times %v and %v", tc.kind, loaded.ExpiresAt, loaded.IdleExpiresAt())
		}
	}

	// a guest kind is not made here, and the table never holds an admin session for an admin that does not exist
	if _, _, err := store.CreateAdmin(ctx, adm, session.KindGuest, nil, false); err == nil {
		t.Fatal("a guest kind was accepted")
	}

	if _, _, err := store.CreateAdmin(ctx, session.Admin{ID: "00000000-0000-4000-8000-000000000404"}, session.KindAdmin, nil, false); err == nil {
		t.Fatal("a session for an admin that does not exist was stored")
	}
}

func TestCreateAdminEndsTheSessionsThatItShould(t *testing.T) {
	store, pool, _ := setup(t)
	ctx := context.Background()
	adm := insertAdmin(t, pool, "admin@example.org")
	other := insertAdmin(t, pool, "other@example.org")

	guestToken, guest, _ := store.CreateGuest(ctx, nil, nil)
	_, first, _ := store.CreateAdmin(ctx, adm, session.KindAdmin, nil, false)
	_, second, _ := store.CreateAdmin(ctx, adm, session.KindAdmin, nil, false)
	_, elsewhere, _ := store.CreateAdmin(ctx, other, session.KindAdmin, nil, false)

	if n := countRows(t, pool); n != 4 {
		t.Fatalf("%d sessions exist", n)
	}

	// a sign in replaces the session that came with it, and only that one
	_, third, err := store.CreateAdmin(ctx, adm, session.KindAdmin, guest.TokenHash, false)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	if _, err := store.Load(ctx, guestToken); !errors.Is(err, session.ErrNoSession) {
		t.Fatalf("the guest session that came with the sign in is still there, %v", err)
	}

	if n := countRows(t, pool); n != 4 {
		t.Fatalf("%d sessions exist after the replacement", n)
	}

	// endOthers ends every session of the admin, the one that came with the request and the others, and no session of another admin
	token, fourth, err := store.CreateAdmin(ctx, adm, session.KindAdmin, first.TokenHash, true)
	if err != nil {
		t.Fatalf("create failed %v", err)
	}

	if n := countRows(t, pool); n != 2 {
		t.Fatalf("%d sessions exist, want the new one and the one of the other admin", n)
	}

	if _, err := store.Load(ctx, token); err != nil {
		t.Fatalf("the new session is gone %v", err)
	}

	for _, gone := range []session.Session{first, second, third} {
		if countHash(t, pool, gone.TokenHash) != 0 {
			t.Fatal("a session of the admin survived endOthers")
		}
	}

	if countHash(t, pool, elsewhere.TokenHash) != 1 || countHash(t, pool, fourth.TokenHash) != 1 {
		t.Fatal("the session of the other admin or the new one is gone")
	}
}

func TestDeleteOthers(t *testing.T) {
	store, pool, _ := setup(t)
	ctx := context.Background()
	adm := insertAdmin(t, pool, "admin@example.org")
	other := insertAdmin(t, pool, "other@example.org")

	_, first, _ := store.CreateAdmin(ctx, adm, session.KindAdmin, nil, false)
	_, second, _ := store.CreateAdmin(ctx, adm, session.KindAdminSetup, nil, false)
	_, elsewhere, _ := store.CreateAdmin(ctx, other, session.KindAdmin, nil, false)
	_, guest, _ := store.CreateGuest(ctx, nil, nil)

	n, err := session.DeleteOthers(ctx, pool, adm.ID, first.TokenHash)
	if err != nil || n != 1 {
		t.Fatalf("delete others gave %d and %v", n, err)
	}

	if countHash(t, pool, first.TokenHash) != 1 || countHash(t, pool, second.TokenHash) != 0 ||
		countHash(t, pool, elsewhere.TokenHash) != 1 || countHash(t, pool, guest.TokenHash) != 1 {
		t.Fatal("the wrong sessions were deleted")
	}

	// a nil hash keeps nothing, so every session of the admin goes
	n, err = session.DeleteOthers(ctx, pool, adm.ID, nil)
	if err != nil || n != 1 || countHash(t, pool, first.TokenHash) != 0 {
		t.Fatalf("delete with no kept session gave %d and %v", n, err)
	}

	if countRows(t, pool) != 2 {
		t.Fatal("a session of another admin or a guest was deleted")
	}

	// an admin with no sessions is not an error
	if n, err := session.DeleteOthers(ctx, pool, adm.ID, nil); err != nil || n != 0 {
		t.Fatalf("delete for an admin with no sessions gave %d and %v", n, err)
	}
}
