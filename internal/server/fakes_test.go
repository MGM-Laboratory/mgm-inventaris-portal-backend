package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/ratelimit"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/session"
)

const testOrigin = "https://portal.example.org"

// fakeStore keeps sessions in memory. The database tests of the session package prove the real store.
type fakeStore struct {
	mu       sync.Mutex
	sessions map[string]session.Session
	loadErr  error
	created  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{sessions: map[string]session.Session{}}
}

func (f *fakeStore) CreateGuest(_ context.Context, identity *session.Identity, previous []byte) (string, session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	token, _ := session.NewToken()
	csrf, _ := session.NewToken()
	now := time.Now().UTC().Truncate(time.Second)
	limits, _ := session.LimitsFor(session.KindGuest)

	sess := session.Session{
		TokenHash: session.HashToken(token), Kind: session.KindGuest, Identity: identity, CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(limits.Absolute),
	}

	delete(f.sessions, string(previous))
	f.sessions[string(sess.TokenHash)] = sess
	f.created++

	return token, sess, nil
}

func (f *fakeStore) Load(_ context.Context, token string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.loadErr != nil {
		return session.Session{}, f.loadErr
	}

	sess, ok := f.sessions[string(session.HashToken(token))]
	if !ok {
		return session.Session{}, session.ErrNoSession
	}

	return sess, nil
}

func (f *fakeStore) Delete(_ context.Context, hash []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.sessions, string(hash))

	return nil
}

func (f *fakeStore) has(token string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	_, ok := f.sessions[string(session.HashToken(token))]

	return ok
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.sessions)
}

// add stores a session of any kind and returns its token.
func (f *fakeStore) add(kind session.Kind, identity *session.Identity) (string, session.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()

	token, _ := session.NewToken()
	csrf, _ := session.NewToken()
	now := time.Now().UTC().Truncate(time.Second)
	sess := session.Session{
		TokenHash: session.HashToken(token), Kind: kind, Identity: identity, CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour),
	}

	if kind != session.KindGuest {
		sess.Admin = &session.Admin{ID: "00000000-0000-4000-8000-000000000009", Name: "Example Admin", Email: "admin@example.org"}
	}

	f.sessions[string(sess.TokenHash)] = sess

	return token, sess
}

// fakeLimiter records what the route asks and answers with a fixed verdict.
type fakeLimiter struct {
	mu      sync.Mutex
	keys    []string
	limits  []int
	windows []time.Duration
	over    bool
	retry   time.Duration
	err     error
}

func (f *fakeLimiter) Hit(_ context.Context, key string, limit int, window time.Duration) (ratelimit.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.keys = append(f.keys, key)
	f.limits = append(f.limits, limit)
	f.windows = append(f.windows, window)

	if f.err != nil {
		return ratelimit.Result{}, f.err
	}

	return ratelimit.Result{Count: len(f.keys), Allowed: !f.over, RetryAfter: f.retry}, nil
}

func newTestAPI() *fiber.App {
	return NewAPI(testConfig(), Deps{Sessions: newFakeStore(), Limiter: &fakeLimiter{}})
}

func sameOriginHeaders() map[string]string {
	return map[string]string{"Origin": testOrigin}
}

// send makes a request with a body and returns the answer and its body.
func send(app *fiber.App, method, path, body string, headers map[string]string) (*http.Response, []byte, error) {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := app.Test(req)
	if err != nil {
		return nil, nil, err
	}

	defer func() { _ = res.Body.Close() }()

	data, err := io.ReadAll(res.Body)

	return res, data, err
}

// outageError is a network error, which the routes answer as an outage of the database.
type outageError struct{}

func (*outageError) Error() string   { return "connection refused" }
func (*outageError) Timeout() bool   { return false }
func (*outageError) Temporary() bool { return true }
