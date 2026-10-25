package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStorageModesAndMachinePersistence(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "store")}
	m, e := s.Machine()
	if e != nil {
		t.Fatal(e)
	}
	again, e := s.Machine()
	if e != nil || m.MachineID != again.MachineID || len(m.MachineID) != 36 {
		t.Fatal("unstable machine ID", e)
	}
	if e = s.Save("auth", Auth{AccessToken: "secret", RefreshToken: "r"}); e != nil {
		t.Fatal(e)
	}
	for name, want := range map[string]os.FileMode{"": 0700, "auth.json": 0600, "machine.json": 0600} {
		st, e := os.Stat(filepath.Join(s.Dir, name))
		if e != nil {
			t.Fatal(e)
		}
		if st.Mode().Perm() != want {
			t.Fatalf("%s mode %o", name, st.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(s.Dir)
	if len(entries) != 2 {
		t.Fatal("temporary files left behind")
	}
}
