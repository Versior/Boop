package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"boop/internal/auth"
	"boop/internal/store"
)

// scriptedPrompter answers password prompts from a fixed list, so the command
// can be tested without a terminal.
func scriptedPrompter(values ...string) passwordPrompter {
	index := 0
	return func(label string) (string, error) {
		if index >= len(values) {
			return "", errors.New("prompter has no more answers")
		}
		value := values[index]
		index++
		return value, nil
	}
}

func failingPrompter(err error) passwordPrompter {
	return func(string) (string, error) { return "", err }
}

func TestInitOwnerCreatesTheOwnerAccount(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOOP_DATA_DIR", dataDir)

	var stdout bytes.Buffer
	err := runInitOwner(
		[]string{"--email", " Owner@Example.com ", "--name", " 站长 "},
		&stdout,
		scriptedPrompter("owner-password-1", "owner-password-1"),
	)
	if err != nil {
		t.Fatalf("runInitOwner: %v", err)
	}
	if !strings.Contains(stdout.String(), "owner@example.com") {
		t.Errorf("stdout = %q, want it to name the created owner", stdout.String())
	}

	db, err := store.Open(filepath.Join(dataDir, "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	owner, err := auth.FindByEmail(context.Background(), db, "owner@example.com")
	if err != nil {
		t.Fatalf("FindByEmail: %v", err)
	}
	if !owner.IsOwner() {
		t.Errorf("role = %q, want %q", owner.Role, auth.RoleOwner)
	}
	if owner.DisplayName != "站长" {
		t.Errorf("display_name = %q, want the trimmed name", owner.DisplayName)
	}
}

func TestInitOwnerRefusesToRunTwice(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOOP_DATA_DIR", dataDir)

	args := []string{"--email", "owner@example.com", "--name", "站长"}
	if err := runInitOwner(args, &bytes.Buffer{}, scriptedPrompter("owner-password-1", "owner-password-1")); err != nil {
		t.Fatalf("first run: %v", err)
	}

	var stdout bytes.Buffer
	err := runInitOwner(args, &stdout, scriptedPrompter("owner-password-2", "owner-password-2"))
	if !errors.Is(err, auth.ErrOwnerExists) {
		t.Fatalf("second run error = %v, want ErrOwnerExists", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("refused run still printed %q", stdout.String())
	}

	db, err := store.Open(filepath.Join(dataDir, "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	var users, owners int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*), sum(role = 'owner') FROM users`).Scan(&users, &owners); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if users != 1 || owners != 1 {
		t.Errorf("users = %d, owners = %d, want exactly 1 and 1", users, owners)
	}
}

func TestInitOwnerRequiresEmailAndName(t *testing.T) {
	t.Setenv("BOOP_DATA_DIR", t.TempDir())

	tests := []struct {
		name string
		args []string
		want string
	}{
		{"missing email", []string{"--name", "站长"}, "--email"},
		{"missing name", []string{"--email", "owner@example.com"}, "--name"},
		{"blank email", []string{"--email", "  ", "--name", "站长"}, "--email"},
		{"unknown flag", []string{"--email", "owner@example.com", "--name", "站长", "--admin"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runInitOwner(tt.args, &bytes.Buffer{}, scriptedPrompter("owner-password-1", "owner-password-1"))
			if err == nil {
				t.Fatal("runInitOwner succeeded, want error")
			}
			if tt.want != "" && !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not mention %s", err, tt.want)
			}
		})
	}
}

func TestInitOwnerRejectsPasswordMismatch(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOOP_DATA_DIR", dataDir)

	err := runInitOwner(
		[]string{"--email", "owner@example.com", "--name", "站长"},
		&bytes.Buffer{},
		scriptedPrompter("owner-password-1", "owner-password-2"),
	)
	if err == nil {
		t.Fatal("runInitOwner accepted a mismatched confirmation")
	}

	db, err := store.Open(filepath.Join(dataDir, "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if exists, err := auth.OwnerExists(context.Background(), db); err != nil || exists {
		t.Errorf("OwnerExists = %v (%v), want false: nothing may be created", exists, err)
	}
}

func TestInitOwnerRejectsWeakPassword(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOOP_DATA_DIR", dataDir)

	err := runInitOwner(
		[]string{"--email", "owner@example.com", "--name", "站长"},
		&bytes.Buffer{},
		scriptedPrompter("short", "short"),
	)
	if !errors.Is(err, auth.ErrInvalidPassword) {
		t.Fatalf("error = %v, want ErrInvalidPassword", err)
	}
}

func TestInitOwnerPropagatesPromptFailure(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("BOOP_DATA_DIR", dataDir)

	promptErr := errors.New("stdin is not a terminal")
	err := runInitOwner(
		[]string{"--email", "owner@example.com", "--name", "站长"},
		&bytes.Buffer{},
		failingPrompter(promptErr),
	)
	if !errors.Is(err, promptErr) {
		t.Fatalf("error = %v, want the prompt failure", err)
	}

	db, err := store.Open(filepath.Join(dataDir, "boop.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	if exists, err := auth.OwnerExists(context.Background(), db); err != nil || exists {
		t.Errorf("OwnerExists = %v (%v), want false", exists, err)
	}
}

func TestCommandName(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"boop"}, "serve"},
		{[]string{"boop", "serve"}, "serve"},
		{[]string{"boop", "init-owner", "--email", "a@b.com"}, "init-owner"},
		{[]string{"boop", "-h"}, "serve"},
		{[]string{"boop", "nonsense"}, "nonsense"},
	}
	for _, tt := range tests {
		if got := commandName(tt.args); got != tt.want {
			t.Errorf("commandName(%v) = %q, want %q", tt.args, got, tt.want)
		}
	}
}
