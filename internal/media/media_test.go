package media_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ebnsina/lms-api/internal/media"
)

// The allowlist a deployment might write: one host it has decided to trust.
func embedDriver(t *testing.T) *media.Embed {
	t.Helper()
	return media.NewEmbed([]string{"player.example.test", "Fast.Wistia.NET"})
}

func cloudflareDriver(t *testing.T) *media.Cloudflare {
	t.Helper()

	driver, err := media.NewCloudflare("f33zs165nr7gyfy4")
	if err != nil {
		t.Fatalf("NewCloudflare: %v", err)
	}
	return driver
}

func registry(t *testing.T) *media.Registry {
	t.Helper()
	return media.NewRegistry(embedDriver(t), cloudflareDriver(t))
}

const uid = "6b9e68b07dfee8cc2d116e4c51d6a957"

// Every shape of link an author might paste, and every shape that must be
// refused. The refusals are the point: an `iframe` src is executed content on the
// workspace's own origin.
func TestResolve(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		source    string
		raw       string
		wantURL   string
		wantEmbed string
		wantErr   error
	}{
		// none carries nothing, whatever it is handed. A lesson that stops being a
		// video must not keep a player URL a template could still find.
		{
			name:   "none is understood without a provider",
			source: "none",
		},
		{
			name:   "an empty source is none",
			source: "",
		},
		{
			name:   "none discards a URL",
			source: "none",
			raw:    "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		},

		// YouTube. Each shape yields the same id, and the stored URL is rebuilt from
		// that id rather than kept.
		{
			name:      "a youtube watch link",
			source:    "youtube",
			raw:       "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
		},
		{
			name:      "a youtu.be short link",
			source:    "youtube",
			raw:       "https://youtu.be/dQw4w9WgXcQ",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
		},
		{
			name:      "a youtube embed link",
			source:    "youtube",
			raw:       "https://www.youtube.com/embed/dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		},
		{
			name:      "a youtube short",
			source:    "youtube",
			raw:       "https://youtube.com/shorts/dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		},
		{
			name:      "a mobile youtube link",
			source:    "youtube",
			raw:       "https://m.youtube.com/watch?v=dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
		},
		{
			// The playlist, the timestamp and the tracking parameter are all dropped:
			// the id is the only thing kept, so nothing else can ride into the page.
			name:      "a watch link's extra parameters are discarded",
			source:    "youtube",
			raw:       "https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL123&t=42&si=track",
			wantURL:   "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantEmbed: "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ",
		},
		{
			name:    "a youtube source on somebody else's host",
			source:  "youtube",
			raw:     "https://youtube.com.evil.test/watch?v=dQw4w9WgXcQ",
			wantErr: media.ErrInvalidVideo,
		},
		{
			// Userinfo before an @ makes the host look like YouTube to a reader, and to
			// any check that splits on the wrong character.
			name:    "userinfo cannot forge the youtube host",
			source:  "youtube",
			raw:     "https://www.youtube.com@evil.test/watch?v=dQw4w9WgXcQ",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "a youtube channel page has no video id",
			source:  "youtube",
			raw:     "https://www.youtube.com/@someone",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "an id of the wrong length",
			source:  "youtube",
			raw:     "https://youtu.be/tooshort",
			wantErr: media.ErrInvalidVideo,
		},

		// Vimeo.
		{
			name:      "a vimeo link",
			source:    "vimeo",
			raw:       "https://vimeo.com/123456789",
			wantURL:   "https://vimeo.com/123456789",
			wantEmbed: "https://player.vimeo.com/video/123456789",
		},
		{
			name:      "a vimeo channel link",
			source:    "vimeo",
			raw:       "https://vimeo.com/channels/staffpicks/123456789",
			wantURL:   "https://vimeo.com/123456789",
			wantEmbed: "https://player.vimeo.com/video/123456789",
		},
		{
			name:      "a vimeo player link",
			source:    "vimeo",
			raw:       "https://player.vimeo.com/video/123456789?h=abc",
			wantURL:   "https://vimeo.com/123456789",
			wantEmbed: "https://player.vimeo.com/video/123456789",
		},
		{
			name:    "a vimeo source on somebody else's host",
			source:  "vimeo",
			raw:     "https://evil.test/video/123456789",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "a vimeo link with no id",
			source:  "vimeo",
			raw:     "https://vimeo.com/staffpicks",
			wantErr: media.ErrInvalidVideo,
		},

		// The generic embed, and the allowlist that makes it survivable.
		{
			name:      "an allowed embed host passes through",
			source:    "embed",
			raw:       "https://player.example.test/v/abc?autoplay=1",
			wantURL:   "https://player.example.test/v/abc?autoplay=1",
			wantEmbed: "https://player.example.test/v/abc?autoplay=1",
		},
		{
			name:      "the allowlist is compared case-insensitively",
			source:    "embed",
			raw:       "https://FAST.wistia.net/embed/iframe/abc123",
			wantURL:   "https://FAST.wistia.net/embed/iframe/abc123",
			wantEmbed: "https://FAST.wistia.net/embed/iframe/abc123",
		},
		{
			name:    "an embed host nobody allowed",
			source:  "embed",
			raw:     "https://evil.test/pwn",
			wantErr: media.ErrInvalidVideo,
		},
		{
			// The port is not part of the host, and comparing `Host` rather than
			// `Hostname` would have let this through a list that names evil.test.
			name:    "a port does not smuggle a host past the allowlist",
			source:  "embed",
			raw:     "https://evil.test:443/pwn",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "userinfo does not smuggle a host past the allowlist",
			source:  "embed",
			raw:     "https://player.example.test@evil.test/pwn",
			wantErr: media.ErrInvalidVideo,
		},

		// Schemes. `javascript:` in an iframe src is script execution on this origin;
		// `data:` is a document this origin serves. Both must die at the door.
		{
			name:    "javascript is not a video URL",
			source:  "embed",
			raw:     "javascript:alert(document.cookie)",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "a data URL is not a video URL",
			source:  "embed",
			raw:     "data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "http is not https",
			source:  "youtube",
			raw:     "http://www.youtube.com/watch?v=dQw4w9WgXcQ",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "a relative URL names no host",
			source:  "embed",
			raw:     "/admin",
			wantErr: media.ErrInvalidVideo,
		},
		{
			name:    "a source with no URL",
			source:  "youtube",
			raw:     "",
			wantErr: media.ErrInvalidVideo,
		},

		// Cloudflare Stream. The stored value is the bare id; the account is
		// deployment configuration, so it is not written into every row.
		{
			name:      "a stream video id",
			source:    "hosted",
			raw:       uid,
			wantURL:   uid,
			wantEmbed: "https://customer-f33zs165nr7gyfy4.cloudflarestream.com/" + uid + "/iframe",
		},
		{
			name:      "a stream URL yields its id",
			source:    "hosted",
			raw:       "https://customer-f33zs165nr7gyfy4.cloudflarestream.com/" + uid + "/iframe",
			wantURL:   uid,
			wantEmbed: "https://customer-f33zs165nr7gyfy4.cloudflarestream.com/" + uid + "/iframe",
		},
		{
			name:    "a stream id of the wrong shape",
			source:  "hosted",
			raw:     "not-a-video",
			wantErr: media.ErrInvalidVideo,
		},
		{
			// Traversal in the id would otherwise walk the player URL off the video.
			name:    "a stream id cannot escape its path",
			source:  "hosted",
			raw:     "../../evil",
			wantErr: media.ErrInvalidVideo,
		},

		// An unknown source is refused rather than stored.
		{
			name:    "a source no driver claims",
			source:  "tiktok",
			raw:     "https://www.tiktok.com/@a/video/1",
			wantErr: media.ErrUnsupportedSource,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			video, err := registry(t).Resolve(test.source, test.raw)

			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("Resolve(%q, %q) error = %v, want %v", test.source, test.raw, err, test.wantErr)
				}
				if video != (media.Video{}) {
					t.Errorf("a rejected video should be zero, got %+v", video)
				}
				return
			}

			if err != nil {
				t.Fatalf("Resolve(%q, %q): %v", test.source, test.raw, err)
			}
			if video.URL != test.wantURL {
				t.Errorf("URL = %q, want %q", video.URL, test.wantURL)
			}
			if video.EmbedURL != test.wantEmbed {
				t.Errorf("EmbedURL = %q, want %q", video.EmbedURL, test.wantEmbed)
			}
		})
	}
}

// `none` needs no provider, and every other source needs one. A workspace with no
// video configuration can still write text lessons.
func TestEmptyRegistryStillUnderstandsNone(t *testing.T) {
	t.Parallel()

	empty := media.NewRegistry()

	video, err := empty.Resolve("none", "")
	if err != nil {
		t.Fatalf("Resolve(none): %v", err)
	}
	if video.Source != media.SourceNone || video.EmbedURL != "" {
		t.Errorf("got %+v, want an empty video", video)
	}

	if _, err := empty.Resolve("youtube", "https://youtu.be/dQw4w9WgXcQ"); !errors.Is(err, media.ErrUnsupportedSource) {
		t.Errorf("youtube with no driver: %v, want ErrUnsupportedSource", err)
	}
}

// `embed` renders an author's URL verbatim, so it is the one source that is off
// until a deployment names the hosts it trusts. A driver built with no allowlist
// does not fall back to trusting everything.
func TestEmbedIsDisabledWithoutAnAllowlist(t *testing.T) {
	t.Parallel()

	locked := media.NewRegistry(media.NewEmbed(nil))

	_, err := locked.Resolve("embed", "https://player.example.test/v/abc")
	if !errors.Is(err, media.ErrUnsupportedSource) {
		t.Fatalf("embed with an empty allowlist: %v, want ErrUnsupportedSource", err)
	}

	// YouTube and Vimeo are unaffected: their URLs are written by the driver.
	if _, err := locked.Resolve("youtube", "https://youtu.be/dQw4w9WgXcQ"); err != nil {
		t.Errorf("youtube should not need an allowlist: %v", err)
	}
}

// Every refusal reaches an author eventually, so it has to be a sentence about
// what they typed rather than a stack of package prefixes.
func TestDetailIsTheSentenceAnAuthorReads(t *testing.T) {
	t.Parallel()

	_, err := registry(t).Resolve("embed", "https://evil.test/pwn")
	if err == nil {
		t.Fatal("an unlisted host should have been refused")
	}

	if got, want := media.Detail(err), "evil.test is not a video host this workspace embeds"; got != want {
		t.Errorf("Detail = %q, want %q", got, want)
	}

	// Wrapped by a caller, the sentence still comes out.
	if got := media.Detail(fmt.Errorf("catalog: edit lesson: %w", err)); !strings.HasPrefix(got, "evil.test") {
		t.Errorf("Detail of a wrapped error = %q", got)
	}

	// Anything that did not come from here falls back to its own text.
	if got := media.Detail(errors.New("something else")); got != "something else" {
		t.Errorf("Detail of a foreign error = %q", got)
	}
}

// The customer code is interpolated into a hostname, so it is validated where it
// is configured rather than where it is used.
func TestCloudflareRefusesACustomerCodeThatIsNotOne(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"", "  ", "evil.test/x", "code_with_underscore", "UPPER-CASE"} {
		if _, err := media.NewCloudflare(code); err == nil {
			t.Errorf("NewCloudflare(%q) should have failed", code)
		}
	}

	// The dashboard shows the subdomain, prefix and all. Accepting either spelling
	// is kindness, not laxity: the prefix is stripped and the rest still checked.
	for _, code := range []string{"f33zs165nr7gyfy4", "customer-f33zs165nr7gyfy4", " Customer-F33ZS165NR7GYFY4 "} {
		driver, err := media.NewCloudflare(code)
		if err != nil {
			t.Fatalf("NewCloudflare(%q): %v", code, err)
		}

		video, err := driver.Resolve("hosted", uid)
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if want := "https://customer-f33zs165nr7gyfy4.cloudflarestream.com/" + uid + "/iframe"; video.EmbedURL != want {
			t.Errorf("EmbedURL = %q, want %q", video.EmbedURL, want)
		}
	}
}
