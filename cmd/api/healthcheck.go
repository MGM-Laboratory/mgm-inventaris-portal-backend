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

const (
	// healthAddrEnv is the only setting that the command reads. It is the same variable as the one of the service, with the same
	// default, so the check asks the listener that the service opened. It needs no database setting.
	healthAddrEnv     = "HEALTH_ADDR"
	healthAddrDefault = "127.0.0.1:8081"
	// healthTimeout bounds the whole request, which stays below the timeout of the Docker health check.
	healthTimeout = 3 * time.Second
)

// runHealthcheck asks GET /healthz of the health listener and returns 0 for the answer 200 and 1 for anything else. The image has no
// shell and no curl, so the Docker health check runs this command. It prints only the reason of a failure and never a body.
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
		return fmt.Errorf("no answer from the health listener %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the health route answered %d", resp.StatusCode)
	}

	return nil
}

// healthURL turns the listen address of the health listener into the address of its health route on this host. A listener with no
// host, or with an address that means every interface, is reached on the loopback address.
func healthURL(addr string) (string, error) {
	if addr == "" {
		addr = healthAddrDefault
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", errors.New(healthAddrEnv + " must look like host:port or :port")
	}

	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}

	return "http://" + net.JoinHostPort(host, port) + "/healthz", nil
}
