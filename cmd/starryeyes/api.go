package main

import (
	"net/http"
	"reflect"
	"strconv"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
)

// APIError is the stable error envelope returned by the service.
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
	Containers  []string  `json:"containers" doc:"Supported output containers."`
	VideoCodecs []string  `json:"video_codecs" doc:"Supported video codecs."`
	AudioCodecs []string  `json:"audio_codecs" doc:"Supported audio codecs."`
	Presets     []string  `json:"presets" doc:"Supported output presets."`
	Limits      APILimits `json:"limits" doc:"Server-side media and upload limits."`
}

type APIUploadInstructions struct {
	Mode           string `json:"mode" doc:"Upload protocol mode."`
	ChunkSize      int64  `json:"chunk_size" doc:"Required chunk size in bytes, except for the final chunk."`
	RequiredHeader string `json:"required_header" doc:"Header containing the SHA-256 checksum for each chunk."`
}

type APICreateJobResponse struct {
	ID               string                 `json:"id" doc:"Created job identifier."`
	State            string                 `json:"state" doc:"Initial job state. A new job starts as PENDING while it waits for local spool capacity."`
	ReservationBytes int64                  `json:"reservation_bytes" doc:"Spool capacity currently reserved for this job; zero while pending."`
	Upload           *APIUploadInstructions `json:"upload,omitempty" doc:"Instructions for uploading the input file, present only after admission."`
}

type APIChunkResponse struct {
	Chunk  int    `json:"chunk" doc:"Uploaded chunk number."`
	Bytes  int64  `json:"bytes,omitempty" doc:"Number of bytes accepted for a newly uploaded chunk."`
	Status string `json:"status" doc:"Chunk upload result."`
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
}

func newRouter(s *Server) http.Handler {
	mux := http.NewServeMux()
	config := huma.DefaultConfig("Starryeyes API", "1.0.0")
	// The default transformer adds a $schema member to every JSON response.
	// Keep the established response payloads unchanged while still serving the
	// generated schemas under /schemas.
	config.CreateHooks = nil
	api := humago.New(mux, config)

	registerHumaHandler(api, huma.Operation{
		OperationID: "health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Check service health",
		Responses:   responses(jsonSchema[APIHealthResponse](api), http.StatusOK, "Service is healthy"),
	}, s.health)

	registerHumaHandler(api, huma.Operation{
		OperationID: "getCapabilities",
		Method:      http.MethodGet,
		Path:        "/v1/capabilities",
		Summary:     "Get supported output capabilities",
		Responses:   responses(jsonSchema[APICapabilitiesResponse](api), http.StatusOK, "Supported formats and limits"),
	}, s.cap)

	registerHumaHandler(api, huma.Operation{
		OperationID: "createJob",
		Method:      http.MethodPost,
		Path:        "/v1/jobs",
		Summary:     "Create a media conversion job and place it in the upload-admission queue",
		RequestBody: jsonRequestBody(jsonSchema[Request](api), "Job input and requested output specification"),
		Responses: mergeResponses(
			responses(jsonSchema[APICreateJobResponse](api), http.StatusCreated, "Job accepted in the upload-admission queue"),
			errorResponses(api, http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusInternalServerError),
		),
	}, s.create)

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
	}, s.chunk)

	registerHumaHandler(api, huma.Operation{
		OperationID: "completeJob",
		Method:      http.MethodPost,
		Path:        "/v1/jobs/{id}/complete",
		Summary:     "Finalize an uploaded job",
		Parameters:  []*huma.Param{pathParam("id", "Job identifier.", &huma.Schema{Type: huma.TypeString})},
		Responses: mergeResponses(
			responses(jsonSchema[APIJobStateResponse](api), http.StatusAccepted, "Job finalization started or is already underway"),
			errorResponses(api, http.StatusConflict, http.StatusInternalServerError),
		),
	}, s.complete)

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
	}, s.get)

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
	}, s.output)

	return mux
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
	return &huma.RequestBody{Required: true, Description: description, Content: map[string]*huma.MediaType{
		"application/json": {Schema: schema},
	}}
}

func responses(schema *huma.Schema, status int, description string) map[string]*huma.Response {
	return map[string]*huma.Response{strconv.Itoa(status): {Description: description, Content: map[string]*huma.MediaType{
		"application/json": {Schema: schema},
	}}}
}

func errorResponses(api huma.API, statuses ...int) map[string]*huma.Response {
	schema := jsonSchema[APIError](api)
	result := make(map[string]*huma.Response, len(statuses))
	for _, status := range statuses {
		result[strconv.Itoa(status)] = &huma.Response{Description: http.StatusText(status), Content: map[string]*huma.MediaType{
			"application/json": {Schema: schema},
		}}
	}
	return result
}

func mediaArtifactResponse() map[string]*huma.Response {
	content := map[string]*huma.MediaType{}
	for _, mediaType := range []string{
		"video/mp4",
		"video/webm",
		"video/x-matroska",
		"application/octet-stream",
	} {
		content[mediaType] = &huma.MediaType{Schema: &huma.Schema{Type: huma.TypeString, Format: "binary"}}
	}
	return map[string]*huma.Response{strconv.Itoa(http.StatusOK): {
		Description: "Completed media artifact. The content type depends on the generated container; application/octet-stream is the fallback.",
		Content:     content,
	}}
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
