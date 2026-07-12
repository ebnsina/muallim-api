package catalog_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/ebnsina/muallim-api/internal/catalog"
	"github.com/ebnsina/muallim-api/internal/platform/database"
)

// storedVideo reads the three columns straight out of the table. The service does
// not hand them back, and reading through it would test the reader rather than
// what was written.
func storedVideo(t *testing.T, db *database.DB, tenantID, lessonID uuid.UUID) (source, url, embed string) {
	t.Helper()

	err := db.WithTenantReadOnly(t.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT video_source, video_url, video_embed_url FROM lessons WHERE tenant_id = $1 AND id = $2`,
			tenantID, lessonID).Scan(&source, &url, &embed)
	})
	if err != nil {
		t.Fatalf("read lesson video: %v", err)
	}
	return source, url, embed
}

func videoFixture(t *testing.T) (*database.DB, *catalog.Service, uuid.UUID, uuid.UUID, catalog.Author) {
	t.Helper()

	db := testDB(t)
	svc := newService(db)
	tenantID := seedTenant(t, db)
	author := seedAuthor(t, db, tenantID)
	slug := draftCourse(t, svc, tenantID, author)

	topic, err := svc.AddTopic(t.Context(), tenantID, slug, catalog.NewTopic{Title: "T"}, author)
	if err != nil {
		t.Fatalf("add topic: %v", err)
	}
	return db, svc, tenantID, topic.ID, author
}

// The whole point of the slice. An author pastes a watch link — which does not
// play in a frame — and what is stored is a player URL the server wrote, on the
// no-cookie domain, with the author's tracking parameters gone.
func TestAddLessonStoresAPlayerURLTheServerWrote(t *testing.T) {
	t.Parallel()

	db, svc, tenantID, topicID, author := videoFixture(t)

	lesson, err := svc.AddLesson(t.Context(), tenantID, topicID, catalog.NewLesson{
		Title:       "Lecture one",
		ContentType: "video",
		VideoSource: catalog.VideoYouTube,
		VideoURL:    "https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=PL1&si=track",
	}, author)
	if err != nil {
		t.Fatalf("add lesson: %v", err)
	}

	source, url, embed := storedVideo(t, db, tenantID, lesson.ID)

	if source != catalog.VideoYouTube {
		t.Errorf("source = %q", source)
	}
	if want := "https://www.youtube.com/watch?v=dQw4w9WgXcQ"; url != want {
		t.Errorf("stored URL = %q, want %q — the canonical link, without the playlist or the tracker", url, want)
	}
	if want := "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ"; embed != want {
		t.Errorf("embed URL = %q, want %q", embed, want)
	}
}

// The vulnerability this closes: `video_url` used to reach an `iframe` src
// unchecked, so anybody holding course:write could run a page of their choosing
// on the workspace's origin, for every reader of the lesson.
func TestAVideoURLTheProviderWillNotVouchForIsRefused(t *testing.T) {
	t.Parallel()

	_, svc, tenantID, topicID, author := videoFixture(t)

	hostile := map[string]catalog.NewLesson{
		"a page on an unlisted host": {
			VideoSource: catalog.VideoEmbed, VideoURL: "https://evil.test/steal",
		},
		"a javascript URL": {
			VideoSource: catalog.VideoEmbed, VideoURL: "javascript:fetch('/v1/me')",
		},
		"a data URL": {
			VideoSource: catalog.VideoEmbed, VideoURL: "data:text/html,<script>alert(1)</script>",
		},
		"a link that is not YouTube's": {
			VideoSource: catalog.VideoYouTube, VideoURL: "https://youtube.com.evil.test/watch?v=dQw4w9WgXcQ",
		},
		"a source with no URL at all": {
			VideoSource: catalog.VideoYouTube, VideoURL: "",
		},
		"a source no provider is configured for": {
			VideoSource: catalog.VideoHosted, VideoURL: "6b9e68b07dfee8cc2d116e4c51d6a957",
		},
	}

	for name, lesson := range hostile {
		t.Run(name, func(t *testing.T) {
			lesson.Title, lesson.ContentType = "L", "video"

			if _, err := svc.AddLesson(t.Context(), tenantID, topicID, lesson, author); !errors.Is(err, catalog.ErrInvalidVideo) {
				t.Errorf("AddLesson(%q) err = %v, want ErrInvalidVideo", lesson.VideoURL, err)
			}
		})
	}
}

// A lesson that stops being a video must not keep the player it used to have. A
// template that forgets to check the content type would frame it anyway.
func TestEditingAVideoAwayClearsThePlayer(t *testing.T) {
	t.Parallel()

	db, svc, tenantID, topicID, author := videoFixture(t)

	lesson, err := svc.AddLesson(t.Context(), tenantID, topicID, catalog.NewLesson{
		Title: "L", ContentType: "video",
		VideoSource: catalog.VideoVimeo, VideoURL: "https://vimeo.com/channels/staffpicks/123456789",
	}, author)
	if err != nil {
		t.Fatalf("add lesson: %v", err)
	}

	if _, _, embed := storedVideo(t, db, tenantID, lesson.ID); embed != "https://player.vimeo.com/video/123456789" {
		t.Fatalf("embed URL = %q", embed)
	}

	text, none, empty := "text", catalog.VideoNone, ""
	if _, err := svc.EditLesson(t.Context(), tenantID, lesson.ID, catalog.LessonPatch{
		ContentType: &text, VideoSource: &none, VideoURL: &empty,
	}, author); err != nil {
		t.Fatalf("edit lesson: %v", err)
	}

	source, url, embed := storedVideo(t, db, tenantID, lesson.ID)
	if source != catalog.VideoNone || url != "" || embed != "" {
		t.Errorf("after clearing the video: source=%q url=%q embed=%q — all three should be empty", source, url, embed)
	}
}

// The player URL is derived from the source and the URL together, so a patch may
// not name one without the other. Changing the source alone would leave the three
// columns disagreeing about which video the lesson is.
func TestAPatchMustChangeTheSourceAndTheURLTogether(t *testing.T) {
	t.Parallel()

	db, svc, tenantID, topicID, author := videoFixture(t)

	lesson, err := svc.AddLesson(t.Context(), tenantID, topicID, catalog.NewLesson{
		Title: "L", ContentType: "video",
		VideoSource: catalog.VideoYouTube, VideoURL: "https://youtu.be/dQw4w9WgXcQ",
	}, author)
	if err != nil {
		t.Fatalf("add lesson: %v", err)
	}

	hosted, link := catalog.VideoHosted, "https://vimeo.com/123456789"

	for name, patch := range map[string]catalog.LessonPatch{
		"the source without the URL": {VideoSource: &hosted},
		"the URL without the source": {VideoURL: &link},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := svc.EditLesson(t.Context(), tenantID, lesson.ID, patch, author); !errors.Is(err, catalog.ErrInvalidLesson) {
				t.Errorf("err = %v, want ErrInvalidLesson", err)
			}
		})
	}

	// And the lesson still points at the video it always did.
	if _, _, embed := storedVideo(t, db, tenantID, lesson.ID); embed != "https://www.youtube-nocookie.com/embed/dQw4w9WgXcQ" {
		t.Errorf("a refused patch changed the player: %q", embed)
	}
}

// A patch that leaves the video alone entirely leaves all three columns alone.
// Every other field on the form is still saved.
func TestAPatchThatIgnoresTheVideoKeepsIt(t *testing.T) {
	t.Parallel()

	db, svc, tenantID, topicID, author := videoFixture(t)

	lesson, err := svc.AddLesson(t.Context(), tenantID, topicID, catalog.NewLesson{
		Title: "L", ContentType: "video",
		VideoSource: catalog.VideoEmbed, VideoURL: "https://player.example.test/v/abc",
	}, author)
	if err != nil {
		t.Fatalf("add lesson: %v", err)
	}

	renamed := "Renamed"
	if _, err := svc.EditLesson(t.Context(), tenantID, lesson.ID, catalog.LessonPatch{Title: &renamed}, author); err != nil {
		t.Fatalf("edit lesson: %v", err)
	}

	source, url, embed := storedVideo(t, db, tenantID, lesson.ID)
	if source != catalog.VideoEmbed || url != "https://player.example.test/v/abc" || embed != url {
		t.Errorf("a title-only patch changed the video: source=%q url=%q embed=%q", source, url, embed)
	}
}
