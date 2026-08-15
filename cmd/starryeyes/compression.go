package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	autoMaxTotalBitrate     int64 = 1_600_000
	autoSourceTargetPercent       = int64(85)
	autoSourceMaxPercent          = int64(95)
	autoContainerOverhead         = int64(20_000)
	autoDefaultAudioKbps          = 128
	autoMinimumAudioBps           = int64(16_000)
	autoMinimumVideoBps           = int64(300_000)
)

type compressionResolutionTier struct {
	Name             string
	LandscapeWidth   int
	LandscapeHeight  int
	PortraitWidth    int
	PortraitHeight   int
	RecommendedTotal int64
}

var compressionTiers = [...]compressionResolutionTier{
	{
		Name:             "360p",
		LandscapeWidth:   640,
		LandscapeHeight:  360,
		PortraitWidth:    360,
		PortraitHeight:   640,
		RecommendedTotal: 700_000,
	},
	{
		Name:             "480p",
		LandscapeWidth:   854,
		LandscapeHeight:  480,
		PortraitWidth:    480,
		PortraitHeight:   854,
		RecommendedTotal: 1_000_000,
	},
	{
		Name:             "720p",
		LandscapeWidth:   1280,
		LandscapeHeight:  720,
		PortraitWidth:    720,
		PortraitHeight:   1280,
		RecommendedTotal: 1_400_000,
	},
}

// AutoCompressionPlan is calculated after ffprobe and is persisted indirectly
// through the normalized auto output and the stored probe result. It keeps the
// bitrate budget and the orientation-aware resolution decision together so all
// encoder backends receive the same plan.
type AutoCompressionPlan struct {
	SourceTotalBitrate int64
	TargetTotalBitrate int64
	MaxTotalBitrate    int64
	ResolutionTier     string
	TargetWidth        int
	TargetHeight       int
	VideoBitrate       int64
	VideoMaxrate       int64
	AudioBitrateKbps   int
	Resolution         Resolution
	Copy               bool
}

func autoPlanForJob(job Job, output Output) (*AutoCompressionPlan, error) {
	if job.ProbeJSON == "" {
		return nil, errors.New("auto compression requires stored probe data")
	}
	var probe Probe
	if err := json.Unmarshal([]byte(job.ProbeJSON), &probe); err != nil {
		return nil, fmt.Errorf("decode stored probe: %w", err)
	}
	plan, err := autoCompressionPlan(job.Size, probe, output)
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

func fallbackAutoCompressionPlan(output Output) *AutoCompressionPlan {
	audioBps := int64(output.Audio.BitrateKbps) * 1000
	if audioBps <= 0 {
		audioBps = autoDefaultAudioKbps * 1000
	}
	target := int64(1_200_000)
	video := target - audioBps - autoContainerOverhead
	if video < 1_000 {
		video = 1_000
	}
	return &AutoCompressionPlan{
		SourceTotalBitrate: target,
		TargetTotalBitrate: target,
		MaxTotalBitrate:    autoMaxTotalBitrate,
		TargetWidth:        1280,
		TargetHeight:       720,
		VideoBitrate:       video,
		VideoMaxrate:       autoMaxTotalBitrate - audioBps - autoContainerOverhead,
		AudioBitrateKbps:   int(audioBps / 1000),
		Resolution:         Resolution{Mode: "source"},
	}
}

func autoCompressionPlan(inputSize int64, probe Probe, output Output) (AutoCompressionPlan, error) {
	video, ok := probeVideo(probe)
	if !ok {
		return AutoCompressionPlan{}, errors.New("probe has no video stream")
	}
	duration, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || duration <= 0 {
		return AutoCompressionPlan{}, errors.New("probe has invalid duration")
	}
	displayWidth, displayHeight := probeDisplayDimensions(video)
	if inputSize <= 0 || displayWidth <= 0 || displayHeight <= 0 {
		return AutoCompressionPlan{}, errors.New("probe has invalid video dimensions or input size")
	}

	sourceTotal := inputTotalBitrate(inputSize, duration)
	if sourceTotal <= 0 {
		return AutoCompressionPlan{}, errors.New("input has no usable total bitrate")
	}
	targetTotal := minBitrate(sourceTotal*autoSourceTargetPercent/100, autoMaxTotalBitrate)
	maxTotal := minBitrate(sourceTotal*autoSourceMaxPercent/100, autoMaxTotalBitrate)
	if maxTotal < targetTotal {
		maxTotal = targetTotal
	}

	audioBps := int64(output.Audio.BitrateKbps) * 1000
	if audioBps <= 0 {
		audioBps = autoDefaultAudioKbps * 1000
	}
	videoTarget := targetTotal - audioBps - autoContainerOverhead
	videoMaxrate := maxTotal - audioBps - autoContainerOverhead
	if videoTarget < autoMinimumVideoBps && targetTotal > autoContainerOverhead+autoMinimumAudioBps+autoMinimumVideoBps {
		// Keep the usual 128 kbps audio whenever the source budget allows it,
		// but give video a useful floor for unusually small source bitrates.
		audioBps = targetTotal - autoContainerOverhead - autoMinimumVideoBps
		audioBps = (audioBps / 1000) * 1000
		if audioBps < autoMinimumAudioBps {
			audioBps = autoMinimumAudioBps
		}
		videoTarget = targetTotal - audioBps - autoContainerOverhead
		videoMaxrate = maxTotal - audioBps - autoContainerOverhead
	}
	if videoTarget <= 0 {
		// There is not enough budget for a useful re-encode. A stream copy is
		// used only when it already satisfies the auto resolution and codec
		// defaults; otherwise keep a valid, very small transcode request.
		tier := chooseCompressionTier(targetTotal)
		targetWidth, targetHeight := compressionTargetDimensions(output.Video.Resolution, tier, displayWidth, displayHeight)
		plan := AutoCompressionPlan{
			SourceTotalBitrate: sourceTotal,
			TargetTotalBitrate: targetTotal,
			MaxTotalBitrate:    maxTotal,
			ResolutionTier:     tier.Name,
			TargetWidth:        targetWidth,
			TargetHeight:       targetHeight,
			AudioBitrateKbps:   int(maxInt64(audioBps, autoMinimumAudioBps) / 1000),
			Resolution:         autoPlanResolution(output.Video.Resolution, targetWidth, targetHeight),
			Copy:               autoCopySafe(probe, output, displayWidth, displayHeight, targetWidth, targetHeight),
		}
		if plan.Copy {
			// Stream copy preserves the source bitrate. Reflect that exception in
			// the plan instead of reporting a budget that the output cannot meet.
			plan.TargetTotalBitrate = sourceTotal
			plan.MaxTotalBitrate = sourceTotal
			return plan, nil
		}
		videoTarget = 1_000
		videoMaxrate = videoTarget
	}
	if videoMaxrate < videoTarget {
		videoMaxrate = videoTarget
	}

	tier := chooseCompressionTier(targetTotal)
	targetWidth, targetHeight := compressionTargetDimensions(output.Video.Resolution, tier, displayWidth, displayHeight)
	return AutoCompressionPlan{
		SourceTotalBitrate: sourceTotal,
		TargetTotalBitrate: targetTotal,
		MaxTotalBitrate:    maxTotal,
		ResolutionTier:     tier.Name,
		TargetWidth:        targetWidth,
		TargetHeight:       targetHeight,
		VideoBitrate:       videoTarget,
		VideoMaxrate:       videoMaxrate,
		AudioBitrateKbps:   int(audioBps / 1000),
		Resolution:         autoPlanResolution(output.Video.Resolution, targetWidth, targetHeight),
	}, nil
}

func inputTotalBitrate(inputSize int64, duration float64) int64 {
	if inputSize <= 0 || duration <= 0 {
		return 0
	}
	return int64(float64(inputSize) * 8 / duration)
}

func chooseCompressionTier(targetTotal int64) compressionResolutionTier {
	switch {
	case targetTotal >= 1_250_000:
		return compressionTiers[2]
	case targetTotal >= 850_000:
		return compressionTiers[1]
	default:
		return compressionTiers[0]
	}
}

func compressionBounds(tier compressionResolutionTier, width, height int) (int, int) {
	if height > width {
		return tier.PortraitWidth, tier.PortraitHeight
	}
	return tier.LandscapeWidth, tier.LandscapeHeight
}

func compressionTargetDimensions(requested Resolution, tier compressionResolutionTier, sourceWidth, sourceHeight int) (int, int) {
	switch requested.Mode {
	case "source":
		return sourceWidth, sourceHeight
	case "fit":
		return requested.Width, requested.Height
	default:
		return fitCompressionBounds(tier, sourceWidth, sourceHeight)
	}
}

func fitCompressionBounds(tier compressionResolutionTier, sourceWidth, sourceHeight int) (int, int) {
	boxWidth, boxHeight := compressionBounds(tier, sourceWidth, sourceHeight)
	if sourceWidth <= boxWidth && sourceHeight <= boxHeight {
		return evenAtMost(sourceWidth, sourceWidth), evenAtMost(sourceHeight, sourceHeight)
	}

	var targetWidth, targetHeight int
	if int64(sourceWidth)*int64(boxHeight) > int64(sourceHeight)*int64(boxWidth) {
		targetWidth = boxWidth
		targetHeight = roundedEven(int64(sourceHeight)*int64(boxWidth), int64(sourceWidth))
	} else {
		targetHeight = boxHeight
		targetWidth = roundedEven(int64(sourceWidth)*int64(boxHeight), int64(sourceHeight))
	}
	return evenAtMost(targetWidth, minInt(sourceWidth, boxWidth)), evenAtMost(targetHeight, minInt(sourceHeight, boxHeight))
}

func roundedEven(numerator, denominator int64) int {
	if numerator <= 0 || denominator <= 0 {
		return 0
	}
	return int(math.Round(float64(numerator)/float64(denominator)/2) * 2)
}

func evenAtMost(value, limit int) int {
	if value > limit {
		value = limit
	}
	if value%2 != 0 {
		value--
	}
	if value < 2 && limit >= 2 {
		return 2
	}
	return value
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func autoPlanResolution(requested Resolution, width, height int) Resolution {
	if requested.Mode == "source" || requested.Mode == "fit" {
		return requested
	}
	upscale := false
	return Resolution{Mode: "fit", Width: width, Height: height, Upscale: &upscale}
}

func probeVideo(probe Probe) (ProbeStream, bool) {
	for _, stream := range probe.Streams {
		if stream.CodecType == "video" {
			return stream, true
		}
	}
	return ProbeStream{}, false
}

func probeDisplayDimensions(stream ProbeStream) (int, int) {
	width, height := stream.Width, stream.Height
	rotation := probeRotation(stream)
	if rotation == 90 || rotation == 270 {
		width, height = height, width
	}
	return width, height
}

func probeRotation(stream ProbeStream) int {
	rotation := stream.Rotation
	if rotation == 0 {
		for _, sideData := range stream.SideDataList {
			if sideData.Rotation != 0 {
				rotation = sideData.Rotation
				break
			}
		}
	}
	if rotation == 0 && stream.Tags != nil {
		rotation, _ = strconv.Atoi(stream.Tags["rotate"])
	}
	rotation %= 360
	if rotation < 0 {
		rotation += 360
	}
	return rotation
}

func autoCopySafe(probe Probe, output Output, displayWidth, displayHeight, targetWidth, targetHeight int) bool {
	if output.Container != "mp4" || output.Video.Codec != "h264" || output.Audio.Codec != "aac" || output.Video.Resolution.Mode != "auto" {
		return false
	}
	video, ok := probeVideo(probe)
	if !ok || !strings.EqualFold(video.CodecName, "h264") || displayWidth > targetWidth || displayHeight > targetHeight {
		return false
	}
	for _, stream := range probe.Streams {
		if stream.CodecType == "audio" && !strings.EqualFold(stream.CodecName, "aac") {
			return false
		}
	}
	return true
}

func minBitrate(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func bitrateArg(bps int64) string {
	kbps := bps / 1000
	if kbps < 1 {
		kbps = 1
	}
	return fmt.Sprintf("%dk", kbps)
}
