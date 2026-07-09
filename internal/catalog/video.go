package catalog

import "errors"

// ErrInvalidVideo means a lesson's video reference is not one this deployment can
// vouch for: an unrecognised link, a host nobody allowed, a source no provider
// serves.
//
// It is separate from ErrInvalidLesson because it is the one lesson field whose
// validity depends on how the deployment is configured, and because an author
// staring at a rejected URL deserves to be told it was the URL.
var ErrInvalidVideo = errors.New("catalog: the lesson's video is not valid")

// Video is a resolved video reference.
type Video struct {
	// Source is one of the VideoNone…VideoHosted constants.
	Source string

	// URL is the canonical form of what the author supplied, and what is shown back
	// to them when they next edit the lesson.
	URL string

	// EmbedURL is the player, written by a provider. It is the only one of these
	// three that may be rendered into an `iframe`, and it never came from a
	// request body.
	EmbedURL string
}

// VideoResolver turns a source and an author's URL into something safe to frame.
//
// Declared here, by the package that needs it, and implemented in cmd/ over
// internal/media — a domain package may not import a sibling. Implementations
// return an error wrapping ErrInvalidVideo when the author's input cannot be
// resolved.
//
// It is deliberately synchronous and pure. Resolving a video makes no network
// call, so a lesson can be saved while YouTube is down, and a provider cannot
// stall a request holding a transaction open.
type VideoResolver interface {
	ResolveVideo(source, url string) (Video, error)
}

// resolveVideo validates a lesson's video and returns what to store.
//
// An empty source is `none`, and `none` carries nothing: a lesson that stops
// being a video must not keep the player URL it used to have, or a template that
// forgets to check the content type will happily frame it.
func (s *Service) resolveVideo(source, url string) (Video, error) {
	if s.video == nil {
		// A Service built without a resolver would otherwise store whatever it was
		// handed, which is precisely the defect this type exists to prevent.
		return Video{}, errors.New("catalog: no video resolver is configured")
	}
	return s.video.ResolveVideo(source, url)
}
