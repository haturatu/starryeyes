package main

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestNormalizeVideoEncoder(t *testing.T) {
	config := Config{Capacity: 8 << 20, MaxWidth: 7680, MaxHeight: 4320}
	request := Request{Input: Input{Filename: "clip.mp4", Size: 1 << 20}}

	output, err := normalize(request, config)
	if err != nil {
		t.Fatal(err)
	}
	if output.Video.Encoder != "auto" {
		t.Errorf("default encoder = %q, want auto", output.Video.Encoder)
	}

	request.Output.Video.Encoder = "software"
	output, err = normalize(request, config)
	if err != nil {
		t.Fatal(err)
	}
	if output.Video.Encoder != "software" {
		t.Errorf("explicit encoder = %q, want software", output.Video.Encoder)
	}

	request.Output.Video.Encoder = "qsv"
	if _, err = normalize(request, config); err == nil {
		t.Error("normalize accepted an unsupported video encoder")
	}
}

func TestAutoEncoderIncludesSoftwareFallback(t *testing.T) {
	encoders, err := (&Server{}).videoEncoders(Video{Codec: "hevc", Encoder: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoders) == 0 {
		t.Fatal("auto encoder returned no candidates")
	}
	last := encoders[len(encoders)-1]
	if last.hardware || last.mode != "software" || last.name != "libx265" {
		t.Errorf("auto fallback = %#v, want libx265 software", last)
	}
}

func TestVAAPIRenderNodeValidation(t *testing.T) {
	for _, path := range []string{"/dev/dri/renderD128", "/dev/dri/renderD129"} {
		if !vaapiRenderNode(path) {
			t.Errorf("vaapiRenderNode(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"", "/dev/dri/card0", "/tmp/renderD128", "/dev/dri/renderDfoo"} {
		if vaapiRenderNode(path) {
			t.Errorf("vaapiRenderNode(%q) = true, want false", path)
		}
	}
}

func TestFFmpegHardwareEncoderArguments(t *testing.T) {
	data, output := t.TempDir(), t.TempDir()
	server := &Server{cfg: Config{Data: data, Output: output}}
	job := Job{ID: "job-123"}
	requested := Output{
		Container: "mp4",
		Video:     Video{Codec: "hevc", Quality: Quality{Mode: "crf", CRF: 28}, Resolution: Resolution{Mode: "fit", Width: 1920, Height: 1080}},
		Audio:     Audio{Codec: "aac", BitrateKbps: 160},
	}

	vaapi := server.ffmpegCmd(cgroup{}, job, requested, "output.mp4", videoEncoder{mode: "vaapi", name: "hevc_vaapi", devices: []string{"/dev/dri/renderD128"}})
	if !slices.Contains(vaapi.Args, "-vaapi_device") || !slices.Contains(vaapi.Args, "/dev/dri/renderD128") || !slices.Contains(vaapi.Args, "hevc_vaapi") {
		t.Errorf("VA-API command lacks device or encoder: %q", vaapi.Args)
	}
	if !slices.Contains(vaapi.Args, "scale=1920:1080:force_original_aspect_ratio=decrease:force_divisible_by=2,format=nv12,hwupload") {
		t.Errorf("VA-API command lacks safe upload filter: %q", vaapi.Args)
	}
	if !slices.Contains(vaapi.Args, "-qp") || slices.Contains(vaapi.Args, "-crf") {
		t.Errorf("VA-API rate-control arguments = %q, want -qp without -crf", vaapi.Args)
	}
	if !slices.Contains(vaapi.Args, "--gpu-device") {
		t.Errorf("VA-API sandbox command lacks GPU device permission: %q", vaapi.Args)
	}

	software := server.ffmpegCmd(cgroup{}, job, requested, "output.mp4", videoEncoder{mode: "software", name: "libx265"})
	if !slices.Contains(software.Args, "libx265") || !slices.Contains(software.Args, "-crf") {
		t.Errorf("software command = %q, want libx265 and -crf", software.Args)
	}
	if slices.Contains(software.Args, "--gpu-device") {
		t.Errorf("software sandbox command unexpectedly grants a GPU: %q", software.Args)
	}
	if !slices.Contains(software.Args, filepath.Join(output, job.ID, "output.mp4")) {
		t.Errorf("software command writes outside output directory: %q", software.Args)
	}
}
