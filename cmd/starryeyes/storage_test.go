package main

import (
	"path/filepath"
	"testing"
)

func TestConfigFromEnvRequiresSeparateOutputDirectory(t *testing.T) {
	dataDir := t.TempDir()
	outputDir := t.TempDir()
	t.Setenv("DATA_DIR", dataDir)

	t.Setenv("OUTPUT_DIR", "")
	if _, err := configFromEnv(); err == nil {
		t.Error("configFromEnv accepted a missing OUTPUT_DIR")
	}

	t.Setenv("OUTPUT_DIR", "output")
	if _, err := configFromEnv(); err == nil {
		t.Error("configFromEnv accepted a relative OUTPUT_DIR")
	}

	t.Setenv("OUTPUT_DIR", filepath.Join(dataDir, "output"))
	if _, err := configFromEnv(); err == nil {
		t.Error("configFromEnv accepted an OUTPUT_DIR inside DATA_DIR")
	}

	t.Setenv("OUTPUT_DIR", outputDir)
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	if cfg.Data != dataDir || cfg.Output != outputDir {
		t.Errorf("config = %#v, want Data=%q and Output=%q", cfg, dataDir, outputDir)
	}
}

func TestOutputDirUsesConfiguredStorage(t *testing.T) {
	server := &Server{cfg: Config{Data: "/var/lib/starryeyes", Output: "/mnt/output"}}
	if got := server.outputDir("job-123"); got != "/mnt/output/job-123" {
		t.Errorf("outputDir() = %q, want /mnt/output/job-123", got)
	}
}

func TestSpoolReservationExcludesOutputEstimate(t *testing.T) {
	input := int64(1 << 30)
	output, safety, spool := reservation(input, Output{Preset: "archive-av1"})
	if output != input || safety != output/10 {
		t.Errorf("reservation estimates = output %d, safety %d; want %d and %d", output, safety, input, input/10)
	}
	if spool != input {
		t.Errorf("spool reservation = %d, want input bytes %d", spool, input)
	}
}
