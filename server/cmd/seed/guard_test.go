package main

import (
	"strings"
	"testing"
)

// The seed creates pre-verified accounts with a password that is in the source
// tree. Local, that is a demo. Anywhere else it is a published credential.
func TestSeedRefusesADatabaseThatIsNotLocal(t *testing.T) {
	local := []string{
		"postgres://drive:drive@localhost:55432/drive?sslmode=disable",
		"postgres://drive:drive@127.0.0.1:55433/drive?sslmode=disable",
	}
	for _, dsn := range local {
		if err := guardLocalDSN(dsn, false); err != nil {
			t.Errorf("guardLocalDSN(%q): %v", dsn, err)
		}
	}

	remote := "postgres://drive:secret@containers.example.net:5432/railway"
	err := guardLocalDSN(remote, false)
	if err == nil {
		t.Fatal("a remote DSN was accepted without -force")
	}
	if !strings.Contains(err.Error(), "containers.example.net") {
		t.Errorf("error = %q, want it to name the host it refused", err)
	}
	if err := guardLocalDSN(remote, true); err != nil {
		t.Errorf("-force still refused: %v", err)
	}
}
