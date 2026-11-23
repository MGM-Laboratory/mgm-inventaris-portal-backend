package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// healthTimeout bounds the whole request, which stays below the timeout of the Docker health check.
const healthTimeout = 3 * time.Second

const usage = "usage: docs-server healthcheck\n"

// runCommand runs a subcommand and returns the exit code. The only one is healthcheck. It prints to the writer it is given and never
// to the log, because its output is for the person who runs it.
func runCommand(args []string, stderr io.Writer) int {
	if args[0] != "healthcheck" {
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)

		return 2
	}

	return runHealthcheck(args[1:], settingOr(addrEnv, addrDefault), healthTimeout, stderr)
}

// runHealthcheck asks GET /healthz of the server and returns 0 for the answer 200 and 1 for anything else. The image has no shell and
// no curl, so the Docker health check runs this command. It prints only the reason of a failure and never a body.
func runHealthcheck(args []string, addr string, timeout time.Duration, stderr io.Writer) int {
	if len(args) > 0 {
		_, _ = fmt.Fprintf(stderr, "healthcheck takes no argument\n%s", usage)

		return 2
	}

	if err := checkHealth(addr, timeout); err != nil {
		_, _ = fmt.Fprintln(stderr, "healthcheck failed,", err)

		return 1
	}

	return 0
}

func checkHealth(addr string, timeout time.Duration) error {
	target, err := healthURL(addr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
	if err != nil {
		return fmt.Errorf("build the request %w", err)
	}

	// a redirect is not an answer of the health route, so the first response counts
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("no answer from the server %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the health route answered %d", resp.StatusCode)
	}

	return nil
}

// healthURL turns the listen address of the server into the address of its health route on this host. A server with no host, or with an
// address that means every interface, is reached on the loopback address.
func healthURL(addr string) (string, error) {
	if addr == "" {
		addr = addrDefault
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", errors.New(addrEnv + " must look like host:port or :port")
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port) + healthPath, nil
}
