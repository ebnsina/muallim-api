package main

import (
	"errors"
	"fmt"

	"github.com/ebnsina/lms-api/internal/catalog"
	"github.com/ebnsina/lms-api/internal/media"
	"github.com/ebnsina/lms-api/internal/platform/config"
)

// videoResolver adapts internal/media to the interface catalog declares.
//
// It lives here because catalog may not import a sibling domain package, and
// because which providers exist is a deployment decision. This is the seam: the
// only thing catalog knows is that something turns an author's URL into a player
// URL, or refuses.
type videoResolver struct{ registry *media.Registry }

// newVideoResolver assembles the providers this deployment has configured.
//
// A workspace with no Cloudflare account and no embed allowlist still gets
// YouTube and Vimeo, because those need no account: the driver builds their URLs
// from a video id it extracted itself.
func newVideoResolver(cfg config.Config) (videoResolver, error) {
	providers := []media.Provider{media.NewEmbed(cfg.EmbedHosts)}

	if cfg.StreamCustomer != "" {
		stream, err := media.NewCloudflare(cfg.StreamCustomer)
		if err != nil {
			return videoResolver{}, fmt.Errorf("config: LMS_CLOUDFLARE_STREAM_CUSTOMER: %w", err)
		}
		providers = append(providers, stream)
	}

	return videoResolver{registry: media.NewRegistry(providers...)}, nil
}

// ResolveVideo translates media's refusals into the sentinel catalog knows, so
// that httpapi renders them as a 422 rather than as an unexpected 500.
//
// The author-facing sentence survives the crossing; the package prefixes do not.
func (v videoResolver) ResolveVideo(source, url string) (catalog.Video, error) {
	video, err := v.registry.Resolve(source, url)
	if err != nil {
		if errors.Is(err, media.ErrInvalidVideo) || errors.Is(err, media.ErrUnsupportedSource) {
			return catalog.Video{}, fmt.Errorf("%w: %s", catalog.ErrInvalidVideo, media.Detail(err))
		}
		return catalog.Video{}, err
	}

	return catalog.Video{
		Source:   video.Source,
		URL:      video.URL,
		EmbedURL: video.EmbedURL,
	}, nil
}
