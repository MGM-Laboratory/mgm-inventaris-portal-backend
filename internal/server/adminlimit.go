package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/admin"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/errchain"
	"github.com/MGM-Laboratory/mgm-inventaris-portal-backend/internal/password"
)

const (
	// adminFailureWindow is the window of the limits on the failed admin sign ins of one email and address, and on the failed checks
	// of one session. The wait after the limit is the rest of the window, so it is 15 minutes at most.
	adminFailureWindow = 15 * time.Minute
	// adminAddressWindow is the window of the limit on the failed admin sign ins of one address. The wait after it is up to an hour.
	adminAddressWindow = time.Hour
	// refundTimeout bounds the refund of a hit.
	refundTimeout = 5 * time.Second
	// busyRetryAfter is the Retry-After of the answer for a hasher that has no free slot, in seconds.
	busyRetryAfter = 2
)

// limitSpec is one counter that a request hits.
type limitSpec struct {
	key    string
	limit  int
	window time.Duration
}

type hitRecord struct {
	key   string
	start time.Time
}

// reservation holds the hits that a request made on counters that count failures only. The request hits first, so that parallel
// guesses cannot all pass before the first failure is counted. The hits are refunded when the request does not fail.
type reservation struct {
	limiter RateLimiter
	hits    []hitRecord
}

// reserve hits every counter. It returns the 429 error when one is over its limit, and it has refunded every hit by then. A failed
// counter gives the database error and refunds too.
func reserve(ctx context.Context, limiter RateLimiter, specs ...limitSpec) (*reservation, error) {
	r := &reservation{limiter: limiter}

	for _, spec := range specs {
		res, err := limiter.Hit(ctx, spec.key, spec.limit, spec.window)
		if err != nil {
			r.refund(ctx)

			return nil, dbError("count the failed checks", err)
		}

		r.hits = append(r.hits, hitRecord{key: spec.key, start: res.WindowStart})

		if !res.Allowed {
			r.refund(ctx)

			e := NewError(CodeRateLimited)
			e.RetryAfter = res.RetryAfterSeconds()

			return nil, e
		}
	}

	return r, nil
}

// refund takes every hit back. It runs on a context that the caller cannot cancel, so a client that leaves does not keep the hit. A
// refund that fails leaves the count higher, which is the safe side, and it is logged.
func (r *reservation) refund(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), refundTimeout)
	defer cancel()

	for _, h := range r.hits {
		if err := r.limiter.Refund(ctx, h.key, h.start); err != nil {
			slog.Warn("a hit could not be refunded", "error_chain", errchain.Chain(err))
		}
	}

	r.hits = nil
}

// settle ends the reservation with the result of the check. A wrong credential is a failure and keeps its hits. Success, a busy
// hasher and a database error are not failures, so their hits go back.
func (r *reservation) settle(ctx context.Context, err error) {
	if !errors.Is(err, admin.ErrInvalidCredentials) {
		r.refund(ctx)
	}
}

// adminError maps the error of the admin service to the answer. message replaces the text of invalid_credentials, because the
// account routes have no email to name.
func adminError(what, message string, err error) error {
	switch {
	case errors.Is(err, admin.ErrInvalidCredentials):
		e := NewError(CodeInvalidCredentials)
		e.Message = message

		return e
	case errors.Is(err, password.ErrBusy):
		e := NewError(CodeUnavailable)
		e.Message = "The service is busy with other sign ins. Try again in a moment."
		e.RetryAfter = busyRetryAfter

		return e
	case errors.Is(err, admin.ErrNotFound):
		return NewError(CodeSessionExpired)
	default:
		return dbError(what, err)
	}
}
