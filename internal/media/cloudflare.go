package media

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	// A Stream video UID is a 32-character hex string.
	streamUID = regexp.MustCompile(`^[0-9a-f]{32}$`)

	// A customer code is the label of a cloudflarestream.com subdomain. Checked
	// because it is interpolated into a hostname: a code carrying a dot or a slash
	// would let configuration point the player at another origin.
	customerCode = regexp.MustCompile(`^[a-z0-9]{1,63}$`)
)

// Cloudflare resolves the `hosted` source against Cloudflare Stream.
//
// Only the customer code is needed to build a player URL; uploading is a
// different concern with a different credential, and this driver deliberately
// does not hold one. Resolving a video is a pure function of the id and the
// account, so it makes no network call and cannot fail slowly.
type Cloudflare struct {
	customer string
}

// NewCloudflare returns a driver for the given Stream customer code — the label
// in `customer-<code>.cloudflarestream.com`, found on the Stream dashboard.
func NewCloudflare(customer string) (*Cloudflare, error) {
	customer = strings.TrimSpace(strings.ToLower(customer))
	customer = strings.TrimPrefix(customer, "customer-")

	if !customerCode.MatchString(customer) {
		return nil, fmt.Errorf("media: %q is not a Cloudflare Stream customer code", customer)
	}

	return &Cloudflare{customer: customer}, nil
}

// Handles reports the sources this driver resolves.
func (c *Cloudflare) Handles(source string) bool { return source == SourceHosted }

// Resolve accepts a Stream video UID, or any Stream URL containing one, and
// returns the player URL.
func (c *Cloudflare) Resolve(source, raw string) (Video, error) {
	if source != SourceHosted {
		return Video{}, unsupported(source)
	}

	uid := strings.ToLower(raw)

	// An author who copied a URL out of the dashboard rather than the bare id has
	// made an understandable mistake, and the id is right there in the path.
	if strings.Contains(uid, "/") {
		parsed, err := url.Parse(uid)
		if err != nil {
			return Video{}, invalid("that is not a URL")
		}

		uid = ""
		for _, segment := range pathSegments(parsed) {
			if streamUID.MatchString(segment) {
				uid = segment
				break
			}
		}
	}

	if !streamUID.MatchString(uid) {
		return Video{}, invalid("that is not a Cloudflare Stream video id")
	}

	// The canonical URL is the bare id. The account is deployment configuration, so
	// writing it into every row would mean rewriting them all to move accounts.
	return Video{
		Source:   SourceHosted,
		URL:      uid,
		EmbedURL: "https://customer-" + c.customer + ".cloudflarestream.com/" + uid + "/iframe",
	}, nil
}
