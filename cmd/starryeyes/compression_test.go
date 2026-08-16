package main

import (
	"encoding/json"
	"slices"
	"testing"
)

func testProbe(width, height int, duration string, rotation int, videoCodec, audioCodec string) Probe {
	var probe Probe
	probe.Format.Duration = duration
	probe.Streams = []ProbeStream{
		{CodecType: "video", Width: width, Height: height, CodecName: videoCodec, Rotation: rotation},
		{CodecType: "audio", CodecName: audioCodec},
	}
	return probe
}

func TestAutoCompressionPlanUsesTotalBitrateBudget(t *testing.T) {
	probe := testProbe(1920, 1080, "3600", 0, "h264", "aac")
	output := Output{
		Container: "mp4",
		Video:     Video{Codec: "h264", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}},
		Audio:     Audio{Codec: "aac", BitrateKbps: 128},
	}
	plan, err := autoCompressionPlan(600<<20, probe, output)
	if err != nil {
		t.Fatal(err)
	}
	source := inputTotalBitrate(600<<20, 3600)
	wantTarget := source * autoSourceTargetPercent / 100
	if plan.SourceTotalBitrate != source || plan.TargetTotalBitrate != wantTarget {
		t.Fatalf("plan bitrates = %#v, want source=%d target=%d", plan, source, wantTarget)
	}
	if plan.TargetTotalBitrate > autoMaxTotalBitrate || plan.MaxTotalBitrate > autoMaxTotalBitrate {
		t.Fatalf("plan exceeds total bitrate cap: %#v", plan)
	}
	if plan.VideoBitrate != wantTarget-128_000-autoContainerOverhead {
		t.Errorf("video bitrate = %d, want %d", plan.VideoBitrate, wantTarget-128_000-autoContainerOverhead)
	}
	if plan.AudioBitrateKbps != 128 || plan.ResolutionTier != "480p" {
		t.Errorf("plan = %#v, want 128 kbps audio and 480p tier", plan)
	}
	if plan.TargetWidth != 854 || plan.TargetHeight != 480 || plan.Resolution.Mode != "fit" {
		t.Errorf("auto resolution = %#v, want 854x480 fit", plan)
	}
}

func TestAutoCompressionPlanDoesNotUpscaleSmallSource(t *testing.T) {
	probe := testProbe(640, 360, "3600", 0, "h264", "aac")
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}}, Audio: Audio{Codec: "aac", BitrateKbps: 128}}
	plan, err := autoCompressionPlan(2<<30, probe, output)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ResolutionTier != "720p" || plan.TargetWidth != 640 || plan.TargetHeight != 360 {
		t.Fatalf("small-source auto resolution = %#v, want source-sized 640x360", plan)
	}
	if got := scale(plan.Resolution); got != "scale=640:360:force_original_aspect_ratio=decrease:force_divisible_by=2" {
		t.Errorf("small-source scale filter = %q", got)
	}
}

func TestNormalizeDefaultsToAutomaticCompression(t *testing.T) {
	output, err := normalize(Request{Input: Input{Filename: "clip.mp4", Size: 1 << 20}}, Config{Capacity: 2 << 30, MaxWidth: 7680, MaxHeight: 4320})
	if err != nil {
		t.Fatal(err)
	}
	if output.Video.Quality.Mode != "auto" || output.Video.Resolution.Mode != "auto" || output.Audio.BitrateKbps != autoDefaultAudioKbps {
		t.Errorf("default output = %#v, want auto rate/resolution and %d kbps audio", output, autoDefaultAudioKbps)
	}

	output, err = normalize(Request{Input: Input{Filename: "clip.mp4", Size: 1 << 20}, Output: Output{Video: Video{Quality: Quality{Mode: "crf", CRF: 18}}}}, Config{Capacity: 2 << 30, MaxWidth: 7680, MaxHeight: 4320})
	if err != nil {
		t.Fatal(err)
	}
	if output.Video.Quality.Mode != "crf" || output.Video.Resolution.Mode != "auto" {
		t.Errorf("explicit CRF output = %#v, want CRF with the default resolution policy", output)
	}
}

func TestAutoCompressionPlanHandlesPortraitRotation(t *testing.T) {
	probe := testProbe(1920, 1080, "3600", 90, "h264", "aac")
	output := Output{
		Container: "mp4",
		Video:     Video{Codec: "h264", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}},
		Audio:     Audio{Codec: "aac", BitrateKbps: 128},
	}
	plan, err := autoCompressionPlan(2<<30, probe, output)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ResolutionTier != "720p" || plan.TargetWidth != 720 || plan.TargetHeight != 1280 {
		t.Errorf("rotated auto resolution = %#v, want portrait 720p bounds", plan)
	}
	if plan.Resolution.Upscale == nil || *plan.Resolution.Upscale {
		errorMessage := "rotated auto resolution permits upscaling"
		t.Error(errorMessage)
	}
}

func TestAutoCompressionPlanKeepsExtremeAspectRatiosSafe(t *testing.T) {
	probe := testProbe(2560, 1080, "60", 0, "h264", "aac")
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "auto", Value: 0}, Resolution: Resolution{Mode: "auto"}}, Audio: Audio{Codec: "aac", BitrateKbps: 128}}
	plan, err := autoCompressionPlan(2<<30, probe, output)
	if err != nil {
		t.Fatal(err)
	}
	if plan.TargetWidth != 1280 || plan.TargetHeight != 540 {
		t.Fatalf("wide output dimensions = %dx%d, want 1280x540", plan.TargetWidth, plan.TargetHeight)
	}
	if got := scale(plan.Resolution); got != "scale=1280:540:force_original_aspect_ratio=decrease:force_divisible_by=2" {
		t.Errorf("wide scale filter = %q", got)
	}
}

func TestAutoCompressionPlanDoesNotCreateNegativeRates(t *testing.T) {
	probe := testProbe(320, 180, "3600", 0, "h264", "aac")
	output := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}}, Audio: Audio{Codec: "aac", BitrateKbps: 128}}
	plan, err := autoCompressionPlan(32<<20, probe, output)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Copy && (plan.VideoBitrate <= 0 || plan.VideoMaxrate <= 0) {
		t.Fatalf("low-bitrate plan has invalid rates: %#v", plan)
	}
	if plan.Copy && plan.VideoBitrate != 0 {
		t.Fatalf("copy plan still has a video rate: %#v", plan)
	}
	if plan.Copy && (plan.TargetTotalBitrate != plan.SourceTotalBitrate || plan.MaxTotalBitrate != plan.SourceTotalBitrate) {
		t.Fatalf("copy plan reports a re-encode budget: %#v", plan)
	}
}

func TestFFmpegAutoCompressionArguments(t *testing.T) {
	data, outputDir := t.TempDir(), t.TempDir()
	probeBytes, err := json.Marshal(testProbe(1920, 1080, "3600", 0, "h264", "aac"))
	if err != nil {
		t.Fatal(err)
	}
	server := &Server{cfg: Config{Data: data, Output: outputDir}}
	job := Job{ID: "job-auto", Size: 600 << 20, ProbeJSON: string(probeBytes)}
	requested := Output{Container: "mp4", Video: Video{Codec: "h264", Quality: Quality{Mode: "auto"}, Resolution: Resolution{Mode: "auto"}}, Audio: Audio{Codec: "aac", BitrateKbps: 128}}
	command, err := server.ffmpegCmd(cgroup{}, job, requested, "output.mp4", videoEncoder{mode: "software", name: "libx264"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"libx264", "-b:v", "-maxrate", "-bufsize", "-b:a", "128k", "scale=854:480:force_original_aspect_ratio=decrease:force_divisible_by=2"} {
		if !slices.Contains(command.Args, want) {
			t.Errorf("auto command lacks %q: %q", want, command.Args)
		}
	}
	if !slices.Contains(command.Args, "0:a:0?") || slices.Contains(command.Args, "0:a?") {
		t.Errorf("auto command maps unexpected audio streams: %q", command.Args)
	}
	explicit := requested
	explicit.Video.Quality = Quality{Mode: "crf", CRF: 23}
	explicitCommand, err := server.ffmpegCmd(cgroup{}, job, explicit, "output.mp4", videoEncoder{mode: "software", name: "libx264"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(explicitCommand.Args, "0:a?") || slices.Contains(explicitCommand.Args, "0:a:0?") {
		t.Errorf("explicit command changed audio mapping: %q", explicitCommand.Args)
	}
	if slices.Contains(command.Args, "-crf") || slices.Contains(command.Args, "-qp") || slices.Contains(command.Args, "-cq") {
		t.Errorf("auto command unexpectedly uses constant-quality control: %q", command.Args)
	}

	vaapi, err := server.ffmpegCmd(cgroup{}, job, requested, "output.mp4", videoEncoder{mode: "vaapi", name: "h264_vaapi", devices: []string{"/dev/dri/renderD128"}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(vaapi.Args, "-rc_mode") || !slices.Contains(vaapi.Args, "VBR") || slices.Contains(vaapi.Args, "-pix_fmt") {
		t.Errorf("VA-API auto command = %q, want VBR without a software pixel format", vaapi.Args)
	}

	nvenc, err := server.ffmpegCmd(cgroup{}, job, requested, "output.mp4", videoEncoder{mode: "nvenc", name: "h264_nvenc"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(nvenc.Args, "-hwaccel") || !slices.Contains(nvenc.Args, "cuda") || !slices.Contains(nvenc.Args, "-hwaccel_output_format") {
		t.Errorf("NVENC auto command = %q, want CUDA input acceleration", nvenc.Args)
	}
	if !slices.Contains(nvenc.Args, "scale_cuda=854:480:force_original_aspect_ratio=decrease:force_divisible_by=2:format=yuv420p") {
		t.Errorf("NVENC auto command = %q, want CUDA scaling", nvenc.Args)
	}
	if !slices.Contains(nvenc.Args, "-rc") || !slices.Contains(nvenc.Args, "vbr") || slices.Contains(nvenc.Args, "-pix_fmt") {
		t.Errorf("NVENC auto command = %q, want VBR and yuv420p", nvenc.Args)
	}
}
