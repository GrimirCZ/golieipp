package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestRegistryInstanceIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.InstanceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	idAgain, err := s.InstanceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || id != idAgain {
		t.Fatalf("registry identity changed: %q / %q", id, idAgain)
	}
	fresh, err := Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	newID, err := fresh.InstanceID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if newID == id {
		t.Fatal("new database reused the registry identity")
	}
}
