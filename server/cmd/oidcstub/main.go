// Command oidcstub is a test fixture: a fake OpenID Connect provider that mints
// signed identity assertions for whoever asks, so `make e2e` can drive the
// sign-in flow without a real Google client.
//
// It refuses to bind anything but a loopback address, and there is deliberately
// no flag to override that. `make seed` has a -force escape hatch because
// seeding a remote database is at least imaginable; there is no legitimate
// non-local use of a program that hands out signed identity tokens for any
// subject and address the caller names.
//
// Nothing in the shipped server imports it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/rahul-sharma-cs/drive/server/internal/oidcstub"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:9099", "loopback address to serve on")
	clientID := flag.String("client-id", "", "the OAuth client id this stub accepts (required)")
	clientSecret := flag.String("client-secret", "", "the OAuth client secret this stub accepts (required)")
	flag.Parse()

	if err := run(*addr, *clientID, *clientSecret); err != nil {
		log.Fatalf("oidcstub: %v", err)
	}
}

func run(addr, clientID, clientSecret string) error {
	if clientID == "" || clientSecret == "" {
		return errors.New("-client-id and -client-secret are both required")
	}
	if err := guardLoopback(addr); err != nil {
		return err
	}

	stub, err := oidcstub.New(clientID, clientSecret)
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	// The stub publishes exactly this string as its issuer and as the prefix of
	// every endpoint, so it has to be told rather than guess: a client that
	// discovered a different string would refuse the document.
	base := "http://" + ln.Addr().String()
	stub.SetBaseURL(base)

	fmt.Fprintf(os.Stderr, "oidcstub: fake identity provider on %s (test fixture — loopback only)\n", base)

	srv := &http.Server{
		Handler:           stub.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return srv.Serve(ln)
}

// guardLoopback refuses any address that is not on this machine.
//
// The check is on the parsed host, not on the string: "localhost" and every
// address in 127.0.0.0/8 and ::1 pass, and an empty host -- ":9099", which
// binds every interface -- does not, because that is exactly the mistake this
// exists to catch.
func guardLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-addr %q: not a host:port address: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf(
		"refusing to serve on %q: this fixture signs identity tokens for any subject it is asked for, "+
			"which belongs on loopback and nowhere else. There is no flag to override this",
		addr)
}
