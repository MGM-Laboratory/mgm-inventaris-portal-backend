package server

import (
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

// requestIDHeader carries the request id in and out.
const requestIDHeader = "X-Request-Id"

type requestIDKey struct{}

// safeRequestID is the form of a client value that is kept. Anything else is replaced, so a header cannot put free text in a log.
var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// RequestID keeps a safe X-Request-Id of the client and makes a UUID otherwise. The id is in the answer header and in the error body.
func RequestID() fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Get(requestIDHeader)
		if !safeRequestID.MatchString(id) {
			id = uuid.NewString()
		}

		c.Locals(requestIDKey{}, id)
		c.Set(requestIDHeader, id)

		return c.Next()
	}
}

func requestID(c fiber.Ctx) string {
	id, _ := c.Locals(requestIDKey{}).(string)

	return id
}

// routePath is the pattern of the route that ran and never the path of the request, because a path can hold a ticket number.
func routePath(c fiber.Ctx) string {
	if route := c.Route(); route != nil {
		return route.Path
	}

	return "unmatched"
}

// NoStore marks every answer, errors included, as not cacheable.
func NoStore() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set(fiber.HeaderCacheControl, "no-store")

		return c.Next()
	}
}

// Recover turns a panic of a handler into the error internal, so one bad request cannot stop the copy.
func Recover() fiber.Handler {
	return func(c fiber.Ctx) (err error) {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("a route panicked", "request_id", requestID(c), "method", c.Method(), "path", routePath(c),
					"panic_type", fmt.Sprintf("%T", r))

				err = NewError(CodeInternal)
			}
		}()

		return c.Next()
	}
}

// AccessLog writes one JSON line for each request. It never writes the query, a header, a cookie or a body.
func AccessLog() fiber.Handler {
	return func(c fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		status := c.Response().StatusCode()
		if err != nil {
			_, status = resolve(err)
		}

		slog.Info("request", "request_id", requestID(c), "method", c.Method(), "path", routePath(c), "status", status,
			"duration_ms", time.Since(start).Milliseconds())

		return err
	}
}
