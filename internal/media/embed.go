package media

import (
	"net/url"
	"regexp"
	"strings"
)

// Embed resolves the video sources that live on somebody else's servers:
// YouTube, Vimeo, and — for a workspace that has decided to allow it — an
// arbitrary player on a named host.
//
// Nothing an author types reaches an `iframe`. A YouTube link becomes an id, and
// the id becomes a URL this driver writes. The `embed` source is the exception,
// and it is exactly why the allowlist exists: it is off unless a deployment names
// the hosts it trusts.
type Embed struct {
	allowedHosts map[string]struct{}
}

// NewEmbed returns a driver that will serve YouTube and Vimeo, and will accept a
// raw `embed` URL only on one of the given hosts.
//
// An empty list disables the `embed` source. That is a safe default and not a
// broken one: `embed` means "render whatever URL an author pasted", so a
// deployment that has not thought about which hosts it trusts has not earned it.
func NewEmbed(allowedHosts []string) *Embed {
	allowed := make(map[string]struct{}, len(allowedHosts))
	for _, host := range allowedHosts {
		host = strings.TrimSpace(strings.ToLower(host))
		if host != "" {
			allowed[host] = struct{}{}
		}
	}
	return &Embed{allowedHosts: allowed}
}

// Handles reports the sources this driver resolves.
func (e *Embed) Handles(source string) bool {
	switch source {
	case SourceYouTube, SourceVimeo, SourceEmbed:
		return true
	}
	return false
}

// Resolve validates the author's URL and returns the player to embed.
func (e *Embed) Resolve(source, raw string) (Video, error) {
	if raw == "" {
		return Video{}, invalid("a %s lesson needs a video URL", source)
	}

	parsed, err := parseHTTPS(raw)
	if err != nil {
		return Video{}, err
	}

	switch source {
	case SourceYouTube:
		return e.youtube(parsed)
	case SourceVimeo:
		return e.vimeo(parsed)
	case SourceEmbed:
		return e.embed(parsed)
	}

	return Video{}, unsupported(source)
}

// parseHTTPS rejects everything that is not an absolute https URL.
//
// http would be a mixed-content warning; `javascript:` and `data:` in an iframe
// src are script execution on this origin. A scheme allowlist of exactly one is
// the only version of this check that cannot be talked around.
func parseHTTPS(raw string) (*url.URL, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, invalid("that is not a URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return nil, invalid("a video URL must begin with https://")
	}
	if parsed.Host == "" {
		return nil, invalid("a video URL must name a host")
	}
	return parsed, nil
}

// host returns the lowercased hostname, without a port or userinfo.
//
// `parsed.Host` keeps the port, and comparing that against an allowlist lets
// `evil.test:443` past a list that names `evil.test` — or, worse, lets
// `player.vimeo.com@evil.test` past by way of userinfo, which `Hostname` strips.
func host(parsed *url.URL) string {
	return strings.ToLower(parsed.Hostname())
}

var (
	// A YouTube id is eleven characters of the URL-safe alphabet.
	youtubeID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

	// A Vimeo id is a run of digits.
	vimeoID = regexp.MustCompile(`^[0-9]{6,12}$`)
)

// youtube accepts every shape of link a person is likely to paste, and keeps the
// id.
//
// The canonical URL is rebuilt from that id rather than kept, so a link carrying a
// playlist, a referrer, or a tracking parameter cannot smuggle any of them into
// the page. The player is on youtube-nocookie.com: an embed on the ordinary
// domain sets advertising cookies for every reader of the lesson, which is a
// decision no author should be making on the workspace's behalf.
func (e *Embed) youtube(parsed *url.URL) (Video, error) {
	segments := pathSegments(parsed)

	var id string
	switch h := host(parsed); h {
	case "youtu.be":
		if len(segments) > 0 {
			id = segments[0]
		}

	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com",
		"youtube-nocookie.com", "www.youtube-nocookie.com":

		if len(segments) > 0 {
			switch segments[0] {
			case "watch":
				id = parsed.Query().Get("v")
			case "embed", "shorts", "live", "v":
				if len(segments) > 1 {
					id = segments[1]
				}
			}
		}

	default:
		return Video{}, invalid("%s is not a YouTube address", h)
	}

	if !youtubeID.MatchString(id) {
		return Video{}, invalid("that link carries no YouTube video id")
	}

	return Video{
		Source:   SourceYouTube,
		URL:      "https://www.youtube.com/watch?v=" + id,
		EmbedURL: "https://www.youtube-nocookie.com/embed/" + id,
	}, nil
}

// vimeo takes the id out of the link and rebuilds both URLs from it.
//
// Vimeo puts the id in a different position depending on the shape of the link —
// /123456789, /channels/staffpicks/123456789, /video/123456789 — so the id is the
// first segment that looks like one, and a link with no such segment is refused.
func (e *Embed) vimeo(parsed *url.URL) (Video, error) {
	switch h := host(parsed); h {
	case "vimeo.com", "www.vimeo.com", "player.vimeo.com":
	default:
		return Video{}, invalid("%s is not a Vimeo address", h)
	}

	var id string
	for _, segment := range pathSegments(parsed) {
		if vimeoID.MatchString(segment) {
			id = segment
			break
		}
	}

	if id == "" {
		return Video{}, invalid("that link carries no Vimeo video id")
	}

	return Video{
		Source:   SourceVimeo,
		URL:      "https://vimeo.com/" + id,
		EmbedURL: "https://player.vimeo.com/video/" + id,
	}, nil
}

// embed passes a URL through, once its host is one the deployment named.
//
// This is the one source where the embedded URL is the author's, so it is the one
// source that can be turned off — and is, by default.
func (e *Embed) embed(parsed *url.URL) (Video, error) {
	if len(e.allowedHosts) == 0 {
		return Video{}, unsupported(SourceEmbed)
	}

	if _, ok := e.allowedHosts[host(parsed)]; !ok {
		return Video{}, invalid("%s is not a video host this workspace embeds", host(parsed))
	}

	// The fragment never reaches the server and has no business in an iframe src;
	// userinfo in one is a phishing device and nothing else.
	parsed.Fragment = ""
	parsed.User = nil

	embedded := parsed.String()

	return Video{Source: SourceEmbed, URL: embedded, EmbedURL: embedded}, nil
}

// pathSegments splits a URL path into its non-empty, unescaped segments.
func pathSegments(parsed *url.URL) []string {
	var segments []string
	for segment := range strings.SplitSeq(parsed.EscapedPath(), "/") {
		unescaped, err := url.PathUnescape(segment)
		if err != nil {
			continue
		}
		if unescaped != "" {
			segments = append(segments, unescaped)
		}
	}
	return segments
}
