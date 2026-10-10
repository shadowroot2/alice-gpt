package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOptionalRoleFile(t *testing.T) {
	t.Setenv("GPT_API_KEY", "test-key")
	t.Setenv("ALICE_SKILL_ID", "test-skill")
	t.Setenv("WEBHOOK_TOKEN", "12345678901234567890123456789012")
	t.Setenv("GPT_MODEL", "test-model")
	path := filepath.Join(t.TempDir(), "role.txt")
	t.Setenv("ROLE_FILE", path)

	cfg, err := LoadConfig()
	if err != nil || cfg.Role != "" {
		t.Fatalf("missing role should be disabled: %q, %v", cfg.Role, err)
	}
	if err := os.WriteFile(path, []byte("Модель: ${GPT_MODEL}."), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig()
	if err != nil || cfg.Role != "Модель: test-model." {
		t.Fatalf("model name was not substituted: %q, %v", cfg.Role, err)
	}
}
