package db

import (
	"errors"
	"testing"
)

// The ownership marker decides whether goose may touch a database at all:
// first boot on a fresh compose volume must work, a foreign Postgres must be
// refused, and an already-migrated database must keep migrating.
func TestDecideMigrate(t *testing.T) {
	cases := []struct {
		name       string
		hasGoose   bool
		tableCount int
		wantErr    bool
	}{
		{name: "fresh compose volume", hasGoose: false, tableCount: 0},
		{name: "already ours", hasGoose: true, tableCount: 15},
		{name: "ours but only the marker", hasGoose: true, tableCount: 1},
		{name: "foreign database", hasGoose: false, tableCount: 1, wantErr: true},
	}
	for _, c := range cases {
		err := decideMigrate(c.hasGoose, c.tableCount)
		if c.wantErr {
			if !errors.Is(err, ErrForeignDatabase) {
				t.Errorf("%s: decideMigrate = %v, want ErrForeignDatabase", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: decideMigrate = %v, want nil", c.name, err)
		}
	}
}
