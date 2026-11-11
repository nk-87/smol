// smol is a one-page demo web server for the team-namho Kargo pipeline.
//
// Two separate things make a release visible on screen, and they move through
// the pipeline differently:
//
//   - defaultColor is compiled into the binary. Change that one line, push,
//     and the build workflow produces a new image. That image becomes Freight,
//     and the page repaints in the new colour as the Freight reaches each
//     Stage. This is the "simulate a release" knob.
//
//   - The release string is injected at deploy time through SMOL_MESSAGE, from
//     the release-infra OpenTofu output in sedemo-platform. It shows a value
//     that infrastructure produced for this Freight, rather than one baked
//     into the image.
//
// Everything is configurable by flag or environment variable; the flag wins.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

// defaultColor is the colour of the banner text.
//
// CHANGE THIS LINE TO CUT A RELEASE. It is baked into the image, so a new
// value means a new image, new Freight, and a visible change at every Stage
// the promotion reaches.
const defaultColor = "orchid"

// gitSHA is stamped at build time with -ldflags "-X main.gitSHA=<sha>".
var gitSHA = "unknown"

// Hex (#rgb to #rrggbbaa) or a bare CSS colour keyword. Anything else is
// rejected rather than interpolated into the stylesheet.
var colorPattern = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20})$`)

type page struct {
	Message string
	Stage   string
	SHA     string
	Color   template.CSS
}

var tmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>smol</title>
<style>
  :root { color-scheme: dark; }
  body {
    margin: 0;
    min-height: 100vh;
    box-sizing: border-box;
    padding: 2rem 1rem;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 2rem;
    background: #0b1020;
    color: #e5e7eb;
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    text-align: center;
  }
  .message {
    color: {{ .Color }};
    font-size: clamp(2.5rem, 11vw, 7rem);
    font-weight: 700;
    letter-spacing: -0.02em;
    line-height: 1.05;
    overflow-wrap: anywhere;
  }
  dl {
    display: grid;
    grid-template-columns: auto auto;
    gap: 0.35rem 1rem;
    margin: 0;
    font-size: clamp(0.8rem, 2.5vw, 1rem);
  }
  dt { color: #6b7280; text-align: right; }
  dd { margin: 0; text-align: left; }
</style>
</head>
<body>
  <div class="message">{{ .Message }}</div>
  <dl>
    <dt>stage</dt><dd>{{ .Stage }}</dd>
    <dt>built from</dt><dd>{{ .SHA }}</dd>
  </dl>
</body>
</html>
`))

// env returns the value of key, or fallback when it is unset or empty. An
// empty value counts as unset so that an env var left blank by a kustomize
// patch falls back to the default rather than blanking the page.
func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// safeColor keeps untrusted input out of the stylesheet. html/template would
// otherwise replace an unrecognised CSS value with ZgotmplZ, which looks like
// a bug on a projector; falling back to the build-time default is clearer.
func safeColor(v string) template.CSS {
	if colorPattern.MatchString(v) {
		return template.CSS(v)
	}
	log.Printf("ignoring invalid colour %q, falling back to %s", v, defaultColor)
	return template.CSS(defaultColor)
}

func main() {
	addr := flag.String("addr", env("SMOL_ADDR", ":8080"), "address to listen on")
	message := flag.String("message", env("SMOL_MESSAGE", ""), "release string to display, from the release-infra OpenTofu output")
	color := flag.String("color", env("SMOL_COLOR", defaultColor), "banner colour: hex or a CSS colour keyword")
	stage := flag.String("stage", env("SMOL_STAGE", ""), "Kargo Stage this instance is running in")
	flag.Parse()

	data := page{
		Message: *message,
		Stage:   *stage,
		SHA:     gitSHA,
		Color:   safeColor(*color),
	}
	if data.Message == "" {
		data.Message = "no release string"
	}
	if data.Stage == "" {
		data.Stage = "unknown"
	}

	mux := http.NewServeMux()

	// "/{$}" matches the root exactly, so anything else 404s instead of
	// falling through to the page.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.Execute(w, data); err != nil {
			log.Printf("render: %v", err)
		}
	})

	// Kept byte-for-byte compatible with the busybox version: the smol-smoke
	// AnalysisTemplate curls /sha/ and the project's "Built from" stage link
	// points at it.
	sha := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "git sha: %s\n", gitSHA)
	}
	mux.HandleFunc("GET /sha", sha)
	mux.HandleFunc("GET /sha/{$}", sha)

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("smol listening on %s (sha=%s stage=%s colour=%s message=%q)",
			*addr, gitSHA, data.Stage, *color, data.Message)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	// Drain in-flight requests on SIGTERM so a rolling update does not show
	// up as a blip in the smoke check.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}
