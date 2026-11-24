// smol is a one-page demo web server for the team-namho Kargo pipeline.
//
// The page shows two things, and they move through the pipeline differently:
//
//   - The version. gitSHA is stamped into the binary at build time and is the
//     same 7 characters the image is tagged with, so the version line names
//     the exact Freight this pod is running. It is rendered in defaultColor,
//     which is compiled in: change that one line, push, and a new image, new
//     Freight and a visibly different version line follow it to every Stage.
//     This is the "simulate a release" knob.
//
//   - The palette. Six swatches, the ANSI terminal colours, plus the page's
//     own foreground and background. They are all CSS custom properties, and
//     their values arrive per Stage from a Catppuccin stylesheet that OpenTofu
//     publishes to S3 during that Stage's promotion. dev runs no OpenTofu, so
//     dev defines none of them and falls back to black on white with six empty
//     outlines -- the visible difference between a Stage with infrastructure
//     behind it and one without.
//
// The two axes are deliberately independent: the version says which release
// this is, the palette says where it is running. The version colour is applied
// inline so a theme can never override it.
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
const defaultColor = "dodgerblue"

// baseCSS is deliberately minimal: layout, and sensible fallbacks for every
// custom property a theme would set. A Stage with no theme renders black on
// white with six empty outlines, which is exactly what dev should look like.
const baseCSS = `
  :root {
    --bg: #ffffff;
    --fg: #000000;
    --muted: #666666;
    --ansi-red: transparent;
    --ansi-green: transparent;
    --ansi-yellow: transparent;
    --ansi-blue: transparent;
    --ansi-magenta: transparent;
    --ansi-cyan: transparent;
  }
  body {
    margin: 0;
    min-height: 100vh;
    box-sizing: border-box;
    padding: 2rem 1rem;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 2.5rem;
    background: var(--bg);
    color: var(--fg);
    font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace;
    text-align: center;
  }
  .version { margin: 0; font-size: clamp(1.5rem, 6vw, 3.5rem); font-weight: 700; }
  .swatches {
    list-style: none; margin: 0; padding: 0;
    display: flex; gap: clamp(0.5rem, 2vw, 1.25rem);
  }
  .swatch {
    width: clamp(2.5rem, 9vw, 6rem);
    aspect-ratio: 1;
    border: 1px solid currentColor;
    border-radius: 4px;
  }
  .swatch.red     { background: var(--ansi-red); }
  .swatch.green   { background: var(--ansi-green); }
  .swatch.yellow  { background: var(--ansi-yellow); }
  .swatch.blue    { background: var(--ansi-blue); }
  .swatch.magenta { background: var(--ansi-magenta); }
  .swatch.cyan    { background: var(--ansi-cyan); }
  dl {
    display: grid; grid-template-columns: auto auto;
    gap: 0.35rem 1rem; margin: 0;
    font-size: clamp(0.8rem, 2.5vw, 1rem);
  }
  dt { color: var(--muted); text-align: right; }
  dd { margin: 0; text-align: left; }
`

// gitSHA is stamped at build time with -ldflags "-X main.gitSHA=<sha>".
var gitSHA = "unknown"

// Hex (#rgb to #rrggbbaa) or a bare CSS colour keyword. Anything else is
// rejected rather than interpolated into the stylesheet.
var colorPattern = regexp.MustCompile(`^(#[0-9a-fA-F]{3,8}|[a-zA-Z]{3,20})$`)

// Only an absolute https URL is linked as a stylesheet. Anything else is
// dropped rather than emitted, so a bad value renders bare instead of putting
// a broken link on a projector.
var themeURLPattern = regexp.MustCompile(`^https://[^\s"'<>]+$`)

type page struct {
	Message  string
	Stage    string
	SHA      string
	Color    template.CSS
	BaseCSS  template.CSS
	ThemeURL template.URL
	ThemeCSS template.CSS
}

var tmpl = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>smol</title>
<style>{{ .BaseCSS }}</style>
{{- with .ThemeURL }}
<link rel="stylesheet" href="{{ . }}">
{{- end }}
{{- with .ThemeCSS }}
<style>{{ . }}</style>
{{- end }}
</head>
<body>
  <p class="version">Version: <span style="color: {{ .Color }}">{{ .SHA }}</span></p>
  <ul class="swatches">
    <li class="swatch red"></li>
    <li class="swatch green"></li>
    <li class="swatch yellow"></li>
    <li class="swatch blue"></li>
    <li class="swatch magenta"></li>
    <li class="swatch cyan"></li>
  </ul>
  <dl>
    <dt>stage</dt><dd>{{ .Stage }}</dd>
    <dt>release</dt><dd>{{ .Message }}</dd>
  </dl>
</body>
</html>
`))

// env returns the value of key, or fallback when it is unset or empty. An
// empty value counts as unset so that an env var left blank in the chart's
// values falls back to the default rather than blanking the page.
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

// safeThemeURL drops anything that is not an absolute https URL, for the same
// reason as safeColor: a visible fallback beats a broken page.
func safeThemeURL(v string) template.URL {
	if v == "" {
		return ""
	}
	if themeURLPattern.MatchString(v) {
		return template.URL(v)
	}
	log.Printf("ignoring invalid theme URL %q, rendering without a theme", v)
	return ""
}

func main() {
	addr := flag.String("addr", env("SMOL_ADDR", ":8080"), "address to listen on")
	message := flag.String("message", env("SMOL_MESSAGE", ""), "release string to display, from the release-infra OpenTofu output")
	color := flag.String("color", env("SMOL_COLOR", defaultColor), "banner colour: hex or a CSS colour keyword")
	stage := flag.String("stage", env("SMOL_STAGE", ""), "Kargo Stage this instance is running in")
	themeURL := flag.String("theme-url", env("SMOL_THEME_URL", ""),
		"stylesheet for this Stage's theme, from the release-infra OpenTofu output")
	themeCSS := flag.String("theme-css", env("SMOL_THEME_CSS", ""),
		"this Stage's theme CSS inlined, for when the theme bucket is not public")
	flag.Parse()

	data := page{
		Message:  *message,
		Stage:    *stage,
		SHA:      gitSHA,
		Color:    safeColor(*color),
		BaseCSS:  template.CSS(baseCSS),
		ThemeURL: safeThemeURL(*themeURL),
		ThemeCSS: template.CSS(*themeCSS),
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
		log.Printf("smol listening on %s (sha=%s stage=%s colour=%s message=%q theme=%q)",
			*addr, gitSHA, data.Stage, *color, data.Message, data.ThemeURL)
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
