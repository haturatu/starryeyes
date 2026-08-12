// gen-docs builds the static OpenAPI site deployed to GitHub Pages.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	apispec "github.com/haturatu/starryeyes/internal/apidoc"
)

const (
	exampleServerURL         = "http://localhost:8080"
	exampleServerDescription = "Example local server — replace with your Starryeyes endpoint"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: gen-docs <output-directory>")
		os.Exit(2)
	}
	if err := generate(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "generate docs:", err)
		os.Exit(1)
	}
}

func generate(outputDir string) error {
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return err
	}
	mux := http.NewServeMux()
	api := humago.New(mux, apispec.Config())
	api.OpenAPI().Servers = []*huma.Server{{
		URL:         exampleServerURL,
		Description: exampleServerDescription,
	}}
	apispec.Register(api, apispec.Handlers{
		Health:       notImplemented,
		Capabilities: notImplemented,
		CreateJob:    notImplemented,
		UploadChunk:  notImplemented,
		CompleteJob:  notImplemented,
		GetJob:       notImplemented,
		Output:       notImplemented,
	})

	openAPI, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outputDir, "openapi.json"), append(openAPI, '\n'), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outputDir, "index.html"), []byte(indexHTML), 0644)
}

func notImplemented(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "documentation generator handler", http.StatusNotImplemented)
}

const indexHTML = `<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Starryeyes API</title>
  </head>
  <body>
    <div id="api-reference"></div>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.64.1"></script>
    <script>
      Scalar.createApiReference('#api-reference', {
        spec: { url: './openapi.json' },
        theme: 'purple',
        hideClientButton: false,
        customCss: '.scalar-api-reference .section:first-of-type { padding-top: 32px; }',
      })
    </script>
  </body>
</html>
`
