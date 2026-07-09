// Package media turns what an author types about a video into something safe to
// put in a page.
//
// It knows nothing about courses, and nothing about HTTP. A caller hands it a
// source and whatever the author supplied — a YouTube link, a Vimeo link, a
// Cloudflare Stream id — and gets back a canonical URL and the URL of a player
// to embed. A source it does not recognise, or a URL it cannot vouch for, is an
// error rather than a string somebody else will render.
//
// That last sentence is the whole point. An `iframe` src is executed content on
// the origin that serves it, so an unvalidated one is a hole through which
// anybody holding course:write reaches every reader of the workspace.
package media

import (
	"errors"
	"fmt"
	"strings"
)

// Video sources. Restated from the schema rather than imported from catalog: a
// domain package may not depend on a sibling, and these strings are enforced by a
// CHECK constraint, not by either package.
const (
	SourceNone    = "none"
	SourceYouTube = "youtube"
	SourceVimeo   = "vimeo"
	SourceEmbed   = "embed"
	SourceHosted  = "hosted"
)

// Sentinel errors.
var (
	// ErrUnsupportedSource means no driver is configured for that source. A
	// workspace with no Cloudflare account cannot host video, and saying so is
	// better than storing an id nothing will ever play.
	ErrUnsupportedSource = errors.New("media: no provider handles that video source")

	// ErrInvalidVideo means the driver recognised the source and could not make
	// sense of what the author supplied.
	ErrInvalidVideo = errors.New("media: the video reference is not valid")
)

// videoError carries a sentinel together with a sentence the author can act on.
//
// Every error this package produces is about something a person typed, so the
// sentence is safe to show them — and a caller in another package needs it
// without the "media:" prefix, since by the time it reaches a client the error is
// about their lesson, not about this package.
type videoError struct {
	sentinel error
	detail   string
}

func (e videoError) Error() string { return "media: " + e.detail }
func (e videoError) Unwrap() error { return e.sentinel }

func invalid(format string, args ...any) error {
	return videoError{sentinel: ErrInvalidVideo, detail: fmt.Sprintf(format, args...)}
}

func unsupported(source string) error {
	return videoError{
		sentinel: ErrUnsupportedSource,
		detail:   fmt.Sprintf("the %s video source is not available here", source),
	}
}

// Detail returns the author-facing sentence of an error from this package, and
// falls back to the error's own text for anything else.
func Detail(err error) string {
	if video, ok := errors.AsType[videoError](err); ok {
		return video.detail
	}
	return err.Error()
}

// Video is a resolved reference: what the author meant, and what to embed.
type Video struct {
	Source string

	// URL is the canonical form of what the author supplied — a tidied link, or a
	// bare Cloudflare id. It is what is shown back to them.
	URL string

	// EmbedURL is the player. It is the only value that should ever reach an
	// `iframe`, and it always comes from a driver rather than from an author.
	EmbedURL string
}

// Provider resolves the sources it handles.
type Provider interface {
	Handles(source string) bool
	Resolve(source, raw string) (Video, error)
}

// Registry dispatches to the first provider that handles a source.
//
// Which providers exist is a deployment decision made in cmd/, not a fact about
// this package. A workspace with no video provider configured refuses every
// source but `none`, which is the correct behaviour and not a degraded one.
type Registry struct {
	providers []Provider
}

// NewRegistry returns a Registry over the given providers, in order.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// Resolve turns a source and an author's input into a Video.
//
// `none` is always understood, needs no provider, and carries nothing: a lesson
// that used to have a video and does not any more must not keep its old player
// URL lying around for a template to find.
func (r *Registry) Resolve(source, raw string) (Video, error) {
	source = strings.TrimSpace(strings.ToLower(source))
	raw = strings.TrimSpace(raw)

	if source == "" || source == SourceNone {
		return Video{Source: SourceNone}, nil
	}

	for _, provider := range r.providers {
		if !provider.Handles(source) {
			continue
		}

		video, err := provider.Resolve(source, raw)
		if err != nil {
			return Video{}, err
		}
		return video, nil
	}

	return Video{}, unsupported(source)
}
