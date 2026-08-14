package main

import (
	"net/http"

	"github.com/danielgtaylor/huma/v2/adapters/humago"
	apispec "github.com/haturatu/starryeyes/internal/apidoc"
)

type APIError = apispec.APIError
type APIHealthResponse = apispec.APIHealthResponse
type APILimits = apispec.APILimits
type APICapabilitiesResponse = apispec.APICapabilitiesResponse
type APIUploadInstructions = apispec.APIUploadInstructions
type APICreateJobResponse = apispec.APICreateJobResponse
type APIChunkResponse = apispec.APIChunkResponse
type APIVerifiedChunk = apispec.APIVerifiedChunk
type APIChunksResponse = apispec.APIChunksResponse
type APIJobStateResponse = apispec.APIJobStateResponse
type APIJobResponse = apispec.APIJobResponse

type Input = apispec.Input
type Request = apispec.Request
type Output = apispec.Output
type Video = apispec.Video
type Quality = apispec.Quality
type Resolution = apispec.Resolution
type Audio = apispec.Audio

func newRouter(s *Server) http.Handler {
	mux := http.NewServeMux()
	api := humago.New(mux, apispec.Config())
	apispec.Register(api, apispec.Handlers{
		Health:       s.health,
		Capabilities: s.cap,
		CreateJob:    s.create,
		ListChunks:   s.listChunks,
		UploadChunk:  s.chunk,
		CompleteJob:  s.complete,
		GetJob:       s.get,
		Output:       s.output,
	})
	return requestLogging(s.log, mux)
}
