package main

import (
	"bytes"
	"errors"
	"net/http"
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

	request.Output.Video.Codec = "vp9"
	request.Output.Video.Encoder = "nvenc"
	request.Output.Container = "webm"
	request.Output.Audio.Codec = "opus"
	if _, err = normalize(request, config); !errors.Is(err, errUnsupportedEncoderCodec) {
		t.Errorf("normalize vp9/nvenc error = %v, want unsupported encoder/codec error", err)
	}
}

func TestCreateRejectsVP9NVENC(t *testing.T) {
	_, server := newAPITestServer(t)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", bytes.NewBufferString(`{"input":{"filename":"clip.webm","size":1048576},"output":{"container":"webm","video":{"codec":"vp9","encoder":"nvenc"},"audio":{"codec":"opus"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "vp9-nvenc-is-not-supported")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("vp9/nvenc create status = %d, want %d", response.StatusCode, http.StatusBadRequest)
	}
}

func TestNVENCCodecSupport(t *testing.T) {
	for codec, want := range map[string]string{"h264": "h264_nvenc", "hevc": "hevc_nvenc", "av1": "av1_nvenc"} {
		got, ok := nvencEncoder(codec)
		if !ok || got != want {
			t.Errorf("nvencEncoder(%q) = %q, %t; want %q, true", codec, got, ok, want)
		}
	}
	if got, ok := nvencEncoder("vp9"); ok || got != "" {
		t.Errorf("nvencEncoder(vp9) = %q, %t; want empty, false", got, ok)
	}
	if _, err := (&Server{}).videoEncoders(Video{Codec: "vp9", Encoder: "nvenc"}); !errors.Is(err, errUnsupportedEncoderCodec) {
		t.Errorf("vp9/nvenc selection error = %v, want unsupported encoder/codec error", err)
	}
}

func TestSoftwareEncoderMapping(t *testing.T) {
	for codec, want := range map[string]string{
		"h264": "libx264",
		"hevc": "libx265",
		"vp9":  "libvpx-vp9",
		"av1":  "libsvtav1",
	} {
		if got := softwareEncoder(codec); got != want {
			t.Errorf("softwareEncoder(%q) = %q, want %q", codec, got, want)
		}
	}
}

func TestAudioEncoderMapping(t *testing.T) {
	for codec, want := range map[string]string{
		"aac":  "aac",
		"opus": "libopus",
		"flac": "flac",
	} {
		if got := audio(codec); got != want {
			t.Errorf("audio(%q) = %q, want %q", codec, got, want)
		}
	}
}

func TestNormalizeQualityAndResolutionModes(t *testing.T) {
	config := Config{Capacity: 8 << 20, MaxWidth: 1920, MaxHeight: 1080}
	base := Request{Input: Input{Filename: "clip.mp4", Size: 1 << 20}}

	tests := []struct {
		name    string
		request Request
		wantErr bool
	}{
		{
			name: "quality lower bound",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "quality", Value: 0}
				return r
			}(),
		},
		{
			name: "quality upper bound",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "quality", Value: 100}
				return r
			}(),
		},
		{
			name: "quality out of range",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "quality", Value: 101}
				return r
			}(),
			wantErr: true,
		},
		{
			name: "crf upper bound for h264",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "crf", CRF: 51}
				return r
			}(),
		},
		{
			name: "crf out of range for h264",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "crf", CRF: 52}
				return r
			}(),
			wantErr: true,
		},
		{
			name: "unsupported quality mode",
			request: func() Request {
				r := base
				r.Output.Video.Quality = Quality{Mode: "bitrate"}
				return r
			}(),
			wantErr: true,
		},
		{
			name: "fit resolution",
			request: func() Request {
				r := base
				r.Output.Video.Resolution = Resolution{Mode: "fit", Width: 1920, Height: 1080}
				return r
			}(),
		},
		{
			name: "resolution out of range",
			request: func() Request {
				r := base
				r.Output.Video.Resolution = Resolution{Mode: "fit", Width: 1, Height: 1080}
				return r
			}(),
			wantErr: true,
		},
		{
			name: "unsupported resolution mode",
			request: func() Request {
				r := base
				r.Output.Video.Resolution = Resolution{Mode: "crop"}
				return r
			}(),
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := normalize(tt.request, config)
			if (err != nil) != tt.wantErr {
				t.Errorf("normalize() error = %v, wantErr %t", err, tt.wantErr)
			}
		})
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
