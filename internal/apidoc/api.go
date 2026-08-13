// Package apidoc owns the Starryeyes HTTP contract and OpenAPI definition.
package apidoc

import (
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

type APIError struct {
	Error string `json:"error" doc:"Human-readable explanation of the failure."`
}

type APIHealthResponse struct {
	OK bool `json:"ok" doc:"Whether the service is ready to accept requests."`
}

type APILimits struct {
	ChunkSize  int64 `json:"chunk_size" doc:"Maximum size of an upload chunk in bytes."`
	MaxWidth   int   `json:"max_width" doc:"Maximum input video width in pixels."`
	MaxHeight  int   `json:"max_height" doc:"Maximum input video height in pixels."`
	MaxStreams int   `json:"max_streams" doc:"Maximum number of media streams in an input."`
}

type APICapabilitiesResponse struct {
	Containers    []string  `json:"containers" doc:"Supported output containers."`
	VideoCodecs   []string  `json:"video_codecs" doc:"Supported video codecs."`
	VideoEncoders []string  `json:"video_encoders" doc:"Video encoder modes accepted by the API. Hardware modes require their device to be exposed to the service."`
	AudioCodecs   []string  `json:"audio_codecs" doc:"Supported audio codecs."`
	Presets       []string  `json:"presets" doc:"Supported output presets."`
	Limits        APILimits `json:"limits" doc:"Server-side media and upload limits."`
}

type APIUploadInstructions struct {
	Mode           string `json:"mode" doc:"Upload protocol mode."`
	ChunkSize      int64  `json:"chunk_size" doc:"Required chunk size in bytes, except for the final chunk."`
	RequiredHeader string `json:"required_header" doc:"Header containing the SHA-256 checksum for each chunk."`
}

type APICreateJobResponse struct {
	ID               string                 `json:"id" doc:"Created job identifier."`
	State            string                 `json:"state" doc:"Current job state. A new job starts as PENDING while it waits for local spool capacity."`
	ReservationBytes int64                  `json:"reservation_bytes" doc:"Spool capacity currently reserved for this job; zero while pending."`
	Upload           *APIUploadInstructions `json:"upload,omitempty" doc:"Instructions for uploading the input file, present only after admission."`
	ExpiresAt        *string                `json:"expires_at,omitempty" doc:"Upload expiration time in RFC 3339 format while the job is resumable."`
}

type APIChunkResponse struct {
	Chunk  int    `json:"chunk" doc:"Uploaded chunk number."`
	Bytes  int64  `json:"bytes,omitempty" doc:"Number of bytes accepted for a newly uploaded chunk."`
	Status string `json:"status" doc:"Chunk upload result."`
}

type APIVerifiedChunk struct {
	Number int    `json:"number" minimum:"0" doc:"Zero-based chunk number."`
	Size   int64  `json:"size" minimum:"1" doc:"Verified chunk size in bytes."`
	SHA256 string `json:"sha256" pattern:"^[0-9a-f]{64}$" doc:"Lowercase hexadecimal SHA-256 of the verified chunk."`
}

type APIChunksResponse struct {
	ChunkSize int64              `json:"chunk_size" minimum:"1" doc:"Required chunk size in bytes, except for the final chunk."`
	Expected  int                `json:"expected" minimum:"1" doc:"Total number of chunks required for the input."`
	Chunks    []APIVerifiedChunk `json:"chunks" doc:"Server-authoritative list of verified chunks, ordered by chunk number."`
}

type APIJobStateResponse struct {
	ID    string `json:"id" doc:"Job identifier."`
	State string `json:"state" doc:"Current job state."`
}

type APIJobResponse struct {
	ID               string                 `json:"id" doc:"Job identifier."`
	State            string                 `json:"state" doc:"Current job state."`
	Filename         string                 `json:"filename" doc:"Original input filename."`
	Size             int64                  `json:"size" doc:"Input size in bytes."`
	BytesReceived    int64                  `json:"bytes_received" doc:"Number of verified upload bytes."`
	ChunksReceived   int                    `json:"chunks_received" doc:"Number of verified chunks."`
	ChunksExpected   int                    `json:"chunks_expected" doc:"Total number of chunks required."`
	ReservationBytes int64                  `json:"reservation_bytes" doc:"Spool capacity currently reserved for this job."`
	Upload           *APIUploadInstructions `json:"upload,omitempty" doc:"Instructions for uploading the input file, present in ADMITTED and UPLOADING states."`
	InputSHA256      string                 `json:"input_sha256" doc:"Verified SHA-256 of the completed input, when available."`
	Error            *string                `json:"error" doc:"Processing failure detail, when the job failed."`
	OutputURL        *string                `json:"output_url" doc:"URL of the completed output artifact, when available."`
	ExpiresAt        *string                `json:"expires_at,omitempty" doc:"Upload expiration time in RFC 3339 format while the job is resumable."`
}

type Input struct {
	Filename string `json:"filename" minLength:"1" doc:"Basename of the uploaded media file. It must be 255 bytes or fewer and contain no path separators or control characters."`
	Size     int64  `json:"size" minimum:"1" doc:"Input file size in bytes. It must not exceed the configured spool capacity."`
	SHA256   string `json:"sha256,omitempty" pattern:"^[0-9a-fA-F]{64}$" doc:"Optional hexadecimal SHA-256 checksum for the complete input file."`
}
type Request struct {
	Input  Input  `json:"input" doc:"Input file metadata."`
	Output Output `json:"output,omitempty" doc:"Optional output selection. Defaults to MP4, H.264, AAC, and source resolution."`
}
type Output struct {
	Preset    string `json:"preset,omitempty" enum:"web-1080p,archive-av1" doc:"Optional preset. Explicit output fields override the preset defaults."`
	Container string `json:"container,omitempty" enum:"mp4,mkv,webm" doc:"Output container. Defaults to mp4."`
	Video     Video  `json:"video,omitempty" doc:"Video encoding options."`
	Audio     Audio  `json:"audio,omitempty" doc:"Audio encoding options."`
}
type Video struct {
	Codec      string     `json:"codec,omitempty" enum:"h264,hevc,av1,vp9" doc:"Video codec. Defaults to h264."`
	Encoder    string     `json:"encoder,omitempty" enum:"auto,software,vaapi,nvenc" doc:"Encoder mode. Defaults to auto, which prefers a usable NVIDIA NVENC or VA-API encoder and retries with software if hardware encoding fails. software, vaapi, and nvenc require that mode and do not fall back."`
	Quality    Quality    `json:"quality,omitempty" doc:"Quality mode. Set value for quality mode or crf for CRF mode."`
	Resolution Resolution `json:"resolution,omitempty" doc:"Resolution mode. Width and height apply only when mode is fit."`
}
type Quality struct {
	Mode  string `json:"mode,omitempty" enum:"quality,crf" doc:"Quality mode. Defaults to quality."`
	Value int    `json:"value,omitempty" minimum:"0" maximum:"100" doc:"Quality value from 0 through 100. Used only when mode is quality."`
	CRF   int    `json:"crf,omitempty" minimum:"0" maximum:"63" doc:"Constant rate factor. Used only when mode is crf; h264 and hevc allow 0 through 51, av1 and vp9 allow 0 through 63."`
}
type Resolution struct {
	Mode    string `json:"mode,omitempty" enum:"source,fit" doc:"Resolution mode. Defaults to source."`
	Width   int    `json:"width,omitempty" minimum:"2" doc:"Target width in pixels when mode is fit; must not exceed the configured maximum width."`
	Height  int    `json:"height,omitempty" minimum:"2" doc:"Target height in pixels when mode is fit; must not exceed the configured maximum height."`
	Upscale *bool  `json:"upscale,omitempty" doc:"Whether fit mode may enlarge the input. Defaults to false."`
}
type Audio struct {
	Codec       string `json:"codec,omitempty" enum:"aac,opus,flac" doc:"Audio codec. Defaults to aac."`
	BitrateKbps int    `json:"bitrate_kbps,omitempty" minimum:"16" maximum:"512" doc:"Audio bitrate in kbps. Defaults to 160."`
}

// Handlers is the runtime implementation for the documented operations.
type Handlers struct {
	Health, Capabilities, CreateJob, ListChunks, UploadChunk, CompleteJob, GetJob, Output http.HandlerFunc
}

// Config returns the shared API configuration without a fixed server URL.
// Runtime documentation therefore targets the server that served it.
func Config() huma.Config {
	config := huma.DefaultConfig("Starryeyes API", "2.0.0")
	// Avoid adding $schema to established JSON responses.
	config.CreateHooks = nil
	return config
}

// Register adds the API contract and its runtime handlers to api.
func Register(api huma.API, handlers Handlers) {
	registerHumaHandler(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Check service health",
		Responses:   responses(jsonSchema[APIHealthResponse](api), http.StatusOK, "Service is healthy"),
	}, handlers.Health)

	registerHumaHandler(api, huma.Operation{
		OperationID: "getCapabilities",
		Method:      http.MethodGet,
		Path:        "/v1/capabilities",
		Summary:     "Get supported output capabilities",
		Responses:   responses(jsonSchema[APICapabilitiesResponse](api), http.StatusOK, "Supported formats and limits"),
	}, handlers.Capabilities)

	registerHumaHandler(api, huma.Operation{
		OperationID: "createJob",
		Method:      http.MethodPost,
		Path:        "/v1/jobs",
		Summary:     "Create a media conversion job and place it in the upload-admission queue",
		Description: "Persist a client-generated idempotency key before this request. Repeating the same key and body returns the same job; reusing a key with a different body returns 409.",
		Parameters: []*huma.Param{
			{Name: "Idempotency-Key", In: "header", Required: true, Description: "Client-generated upload workflow identifier, 16 through 128 characters.", Schema: &huma.Schema{Type: huma.TypeString, MinLength: ptr(16), MaxLength: ptr(128), Pattern: "^[A-Za-z0-9][A-Za-z0-9._:-]{15,127}$"}},
		},
		RequestBody: jsonRequestBody(jsonSchema[Request](api), "Job input and requested output specification"),
		Responses: mergeResponses(
			responses(jsonSchema[APICreateJobResponse](api), http.StatusCreated, "Job created, or the existing job returned for an identical idempotent replay"),
			errorResponses(api, http.StatusBadRequest, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusInternalServerError),
		),
	}, handlers.CreateJob)

	registerHumaHandler(api, huma.Operation{
		OperationID: "listVerifiedChunks",
		Method:      http.MethodGet,
		Path:        "/v1/jobs/{id}/chunks",
		Summary:     "List verified upload chunks",
		Description: "This response is the source of truth when resuming an upload. Clients should upload only chunk numbers absent from the list.",
		Parameters:  []*huma.Param{pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString})},
		Responses: mergeResponses(
			responses(jsonSchema[APIChunksResponse](api), http.StatusOK, "Server-authoritative verified chunks"),
			errorResponses(api, http.StatusNotFound, http.StatusInternalServerError),
		),
	}, handlers.ListChunks)

	registerHumaHandler(api, huma.Operation{
		OperationID: "uploadChunk",
		Method:      http.MethodPut,
		Path:        "/v1/jobs/{id}/chunks/{chunk}",
		Summary:     "Upload and verify one input chunk",
		Description: "The request body must have the exact expected size and include an `X-Chunk-SHA256` header.",
		Parameters: []*huma.Param{
			pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString}),
			pathParam("chunk", "Zero-based chunk number.", &huma.Schema{Type: huma.TypeInteger, Minimum: ptr(float64(0))}),
			{Name: "X-Chunk-SHA256", In: "header", Required: true, Description: "Lowercase or uppercase hexadecimal SHA-256 of the chunk.", Schema: &huma.Schema{Type: huma.TypeString, MinLength: ptr(64), MaxLength: ptr(64), Pattern: "^[0-9a-fA-F]{64}$"}},
		},
		RequestBody: &huma.RequestBody{Required: true, Description: "Raw bytes for exactly one chunk.", Content: map[string]*huma.MediaType{
			"application/octet-stream": {Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}},
		}},
		Responses: mergeResponses(
			responses(jsonSchema[APIChunkResponse](api), http.StatusOK, "Chunk verified or already present"),
			errorResponses(api, http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusRequestedRangeNotSatisfiable, http.StatusInsufficientStorage, http.StatusInternalServerError),
		),
	}, handlers.UploadChunk)

	registerHumaHandler(api, huma.Operation{
		OperationID: "completeJob",
		Method:      http.MethodPost,
		Path:        "/v1/jobs/{id}/complete",
		Summary:     "Finalize an uploaded job",
		Parameters:  []*huma.Param{pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString})},
		Responses: mergeResponses(
			responses(jsonSchema[APIJobStateResponse](api), http.StatusAccepted, "Job finalization started or the job is already finalizing, processing, or completed"),
			errorResponses(api, http.StatusConflict, http.StatusInternalServerError),
		),
	}, handlers.CompleteJob)

	registerHumaHandler(api, huma.Operation{
		OperationID: "getJob",
		Method:      http.MethodGet,
		Path:        "/v1/jobs/{id}",
		Summary:     "Get a job's current status",
		Parameters:  []*huma.Param{pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString})},
		Responses: mergeResponses(
			responses(jsonSchema[APIJobResponse](api), http.StatusOK, "Current job status"),
			errorResponses(api, http.StatusNotFound),
		),
	}, handlers.GetJob)

	registerHumaHandler(api, huma.Operation{
		OperationID: "downloadOutput",
		Method:      http.MethodGet,
		Path:        "/v1/jobs/{id}/output",
		Summary:     "Download a completed output artifact",
		Parameters:  []*huma.Param{pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString})},
		Responses: mergeResponses(
			mediaArtifactResponse(),
			errorResponses(api, http.StatusNotFound, http.StatusInternalServerError),
		),
	}, handlers.Output)
}

func registerHumaHandler(api huma.API, op huma.Operation, handler http.HandlerFunc) {
	api.OpenAPI().AddOperation(&op)
	api.Adapter().Handle(&op, func(ctx huma.Context) {
		request, writer := humago.Unwrap(ctx)
		handler(writer, request)
	})
}

func jsonSchema[T any](api huma.API) *huma.Schema {
	return api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[T](), true, "")
}
func jsonRequestBody(schema *huma.Schema, description string) *huma.RequestBody {
	return &huma.RequestBody{Required: true, Description: description, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
}
func responses(schema *huma.Schema, status int, description string) map[string]*huma.Response {
	return map[string]*huma.Response{strconv.Itoa(status): {Description: description, Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}}
}
func errorResponses(api huma.API, statuses ...int) map[string]*huma.Response {
	schema := jsonSchema[APIError](api)
	result := make(map[string]*huma.Response, len(statuses))
	for _, status := range statuses {
		result[strconv.Itoa(status)] = &huma.Response{Description: http.StatusText(status), Content: map[string]*huma.MediaType{"application/json": {Schema: schema}}}
	}
	return result
}
func mediaArtifactResponse() map[string]*huma.Response {
	content := map[string]*huma.MediaType{}
	for _, mediaType := range []string{"video/mp4", "video/webm", "video/x-matroska", "application/octet-stream"} {
		content[mediaType] = &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}
	}
	return map[string]*huma.Response{strconv.Itoa(http.StatusOK): {Description: "Completed media artifact. The content type depends on the generated container; application/octet-stream is the fallback.", Content: content}}
}
func mergeResponses(groups ...map[string]*huma.Response) map[string]*huma.Response {
	result := map[string]*huma.Response{}
	for _, group := range groups {
		for status, response := range group {
			result[status] = response
		}
	}
	return result
}
func pathParam(name, description string, schema *huma.Schema) *huma.Param {
	return &huma.Param{Name: name, In: "path", Required: true, Description: description, Schema: schema}
}
func ptr[T any](value T) *T { return &value }
