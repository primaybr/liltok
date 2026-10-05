package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayMemories_CRUD(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test_memories.db")
	t.Setenv("LILTOK_SKIP_STARTER_SEED", "1")

	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer database.Close()

	ctx := context.Background()
	project := "/path/to/my/project"

	// 1. Save memories
	m1, err := database.SaveMemory(ctx, project, "convention", "Always write Go unit tests with table-driven tests.")
	if err != nil {
		t.Fatalf("SaveMemory failed: %v", err)
	}
	if m1.ID == "" {
		t.Errorf("expected non-empty ID")
	}

	m2, err := database.SaveMemory(ctx, project, "environment", "PostgreSQL runs on port 5432 with user dev.")
	if err != nil {
		t.Fatalf("SaveMemory 2 failed: %v", err)
	}

	// Global memory
	m3, err := database.SaveMemory(ctx, "global", "quirk", "Windows locks open binaries on rebuild.")
	if err != nil {
		t.Fatalf("SaveMemory 3 failed: %v", err)
	}
	if m3.ID == "" {
		t.Errorf("expected non-empty ID for m3")
	}

	// 2. List memories for project (should include project-specific and global)
	memories, err := database.ListMemories(ctx, project, 10)
	if err != nil {
		t.Fatalf("ListMemories failed: %v", err)
	}
	if len(memories) < 3 {
		t.Errorf("expected at least 3 memories, got %d", len(memories))
	}

	// 3. Format memories for prompt
	promptBlock := FormatMemoriesForPrompt(memories, 1000)
	if !strings.Contains(promptBlock, "[Gateway Memory") {
		t.Errorf("expected [Gateway Memory header, got: %q", promptBlock)
	}
	if !strings.Contains(promptBlock, "table-driven tests") {
		t.Errorf("expected m1 content in prompt block")
	}
	if !strings.Contains(promptBlock, "Windows locks open binaries") {
		t.Errorf("expected m3 global content in prompt block")
	}

	// 4. Delete memory
	if err := database.DeleteMemory(ctx, m2.ID); err != nil {
		t.Fatalf("DeleteMemory failed: %v", err)
	}

	memoriesAfter, err := database.ListMemories(ctx, project, 10)
	if err != nil {
		t.Fatalf("ListMemories after delete failed: %v", err)
	}
	for _, m := range memoriesAfter {
		if m.ID == m2.ID {
			t.Errorf("deleted memory %s still returned", m2.ID)
		}
	}
}
