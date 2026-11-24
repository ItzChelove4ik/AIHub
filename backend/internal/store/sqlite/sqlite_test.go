package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ItzChelove4ik/AIHub/backend/internal/store"
)

func TestBackupWhileOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	db, err := Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(ctx, &store.User{Email: "a@example.com", PasswordHash: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "copy.db")
	if err := Backup(ctx, src, dest); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, src, dest); err == nil {
		t.Fatal("existing backup overwritten")
	}
	cp, err := Open(ctx, dest)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	if n, err := cp.CountUsers(ctx); err != nil || n != 1 {
		t.Fatalf("copy has %d users: %v", n, err)
	}
}
