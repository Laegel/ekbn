package main

import (
	"os"
	"testing"
)

func saveRestoreConfig(t *testing.T) {
	t.Helper()
	orig, err := os.ReadFile("config.yml")
	hasOrig := err == nil
	t.Cleanup(func() {
		if hasOrig {
			os.WriteFile("config.yml", orig, 0644)
		} else {
			os.Remove("config.yml")
		}
	})
}

func writeConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile("config.yml", []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	saveRestoreConfig(t)
	os.Remove("config.yml")
	cfg := loadConfig()
	if cfg.Theme != "dark" {
		t.Errorf("Theme = %q, want %q", cfg.Theme, "dark")
	}
	if cfg.FolderName != "columns" {
		t.Errorf("FolderName = %q, want %q", cfg.FolderName, "columns")
	}
	if cfg.Port != 0 {
		t.Errorf("Port = %d, want 0", cfg.Port)
	}
}

func TestLoadConfigFull(t *testing.T) {
	saveRestoreConfig(t)
	writeConfig(t, "theme: light\nfolder-name: .kanban\nport: 8080\n")
	cfg := loadConfig()
	if cfg.Theme != "light" {
		t.Errorf("Theme = %q, want %q", cfg.Theme, "light")
	}
	if cfg.FolderName != ".kanban" {
		t.Errorf("FolderName = %q, want %q", cfg.FolderName, ".kanban")
	}
	if cfg.Port != 8080 {
		t.Errorf("Port = %d, want 8080", cfg.Port)
	}
}

func TestLoadConfigPartial(t *testing.T) {
	saveRestoreConfig(t)
	writeConfig(t, "theme: synthwave\n")
	cfg := loadConfig()
	if cfg.Theme != "synthwave" {
		t.Errorf("Theme = %q, want %q", cfg.Theme, "synthwave")
	}
	if cfg.FolderName != "columns" {
		t.Errorf("FolderName = %q, want %q", cfg.FolderName, "columns")
	}
	if cfg.Port != 0 {
		t.Errorf("Port = %d, want 0", cfg.Port)
	}
}

func TestLoadConfigMalformedYAML(t *testing.T) {
	saveRestoreConfig(t)
	writeConfig(t, "theme: light\n: broken yaml\n")
	cfg := loadConfig()
	if cfg.Theme != "dark" {
		t.Errorf("Theme = %q, want default %q", cfg.Theme, "dark")
	}
}

func TestLoadConfigPortOnly(t *testing.T) {
	saveRestoreConfig(t)
	writeConfig(t, "port: 9999\n")
	cfg := loadConfig()
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.Theme != "dark" {
		t.Errorf("Theme = %q, want default %q", cfg.Theme, "dark")
	}
}
