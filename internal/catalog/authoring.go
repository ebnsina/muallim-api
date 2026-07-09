package catalog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Authoring errors.
var (
	ErrIncompleteOrder  = errors.New("catalog: the order must list every sibling exactly once")
	ErrEmptyCourse      = errors.New("catalog: a course needs at least one lesson before it can be published")
	ErrAlreadyPublished = errors.New("catalog: the course is already published")
)

// Audit actions this package emits.
const (
	ActionTopicCreated      = "topic.created"
	ActionTopicUpdated      = "topic.updated"
	ActionTopicDeleted      = "topic.deleted"
	ActionTopicsReordered   = "topics.reordered"
	ActionLessonCreated     = "lesson.created"
	ActionLessonUpdated     = "lesson.updated"
	ActionLessonDeleted     = "lesson.deleted"
	ActionLessonsReordered  = "lessons.reordered"
	ActionCoursePublished   = "course.published"
	ActionCourseUnpublished = "course.unpublished"
)

// Video sources a lesson may draw on.
const (
	VideoNone    = "none"
	VideoYouTube = "youtube"
	VideoVimeo   = "vimeo"
	VideoEmbed   = "embed"
	VideoHosted  = "hosted"
)

// NewTopic describes a topic to append to a course.
type NewTopic struct {
	Title string
}

// TopicPatch updates a topic. A nil field is left alone, which is what
// distinguishes "set the title to empty" from "do not touch the title".
type TopicPatch struct {
	Title *string
}

// NewLesson describes a lesson to append to a topic.
type NewLesson struct {
	Title           string
	ContentType     string
	Content         string
	VideoSource     string
	VideoURL        string
	DurationSeconds int
	IsPreview       bool
}

// LessonPatch updates a lesson.
type LessonPatch struct {
	Title           *string
	ContentType     *string
	Content         *string
	VideoSource     *string
	VideoURL        *string
	DurationSeconds *int
	IsPreview       *bool

	// The drip schedule. Read only in the course's own mode: AvailableAt in
	// `scheduled`, AvailableAfterDays in `after_enrolment`, neither in the others.
	//
	// A nil field leaves the column alone, like every other field here, so a
	// schedule cannot be cleared through this patch — only replaced. Switching the
	// course's mode makes it inert, which is the case authors actually have.
	AvailableAt        *time.Time
	AvailableAfterDays *int
}

// AuthoringRepository is the persistence contract for editing a curriculum.
type AuthoringRepository interface {
	CreateTopic(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, n NewTopic) (Topic, error)
	UpdateTopic(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, p TopicPatch) (Topic, error)
	DeleteTopic(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID) (uuid.UUID, error)
	ReorderTopics(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, order []uuid.UUID) error

	CreateLesson(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, n NewLesson) (Lesson, error)
	UpdateLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID, p LessonPatch) (Lesson, error)
	DeleteLesson(ctx context.Context, tx pgx.Tx, tenantID, lessonID uuid.UUID) (uuid.UUID, error)
	ReorderLessons(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID, order []uuid.UUID) error

	TopicByID(ctx context.Context, tx pgx.Tx, tenantID, topicID uuid.UUID) (Topic, error)
	CourseByID(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (Course, error)
	CountLessons(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID) (int, error)
	SetCourseStatus(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, status string) (Course, error)
	SetDripMode(ctx context.Context, tx pgx.Tx, tenantID, courseID uuid.UUID, mode string) (Course, error)
}

// AddTopic appends a topic to a course.
func (s *Service) AddTopic(ctx context.Context, tenantID uuid.UUID, slug string, n NewTopic, author Author) (Topic, error) {
	var created Topic

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}

		created, err = s.authoring.CreateTopic(ctx, tx, tenantID, course.ID, n)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTopicCreated,
			TargetType: "topic", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"course": slug, "title": n.Title},
		})
	})
	if err != nil {
		return Topic{}, err
	}
	return created, nil
}

// EditTopic applies a patch.
func (s *Service) EditTopic(ctx context.Context, tenantID, topicID uuid.UUID, p TopicPatch, author Author) (Topic, error) {
	var updated Topic

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = s.authoring.UpdateTopic(ctx, tx, tenantID, topicID, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTopicUpdated,
			TargetType: "topic", TargetID: topicID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
		})
	})
	if err != nil {
		return Topic{}, err
	}
	return updated, nil
}

// RemoveTopic deletes a topic and, by cascade, its lessons.
//
// The remaining topics are closed up so positions stay dense: a gap is harmless
// today and becomes an off-by-one the first time somebody indexes by position.
func (s *Service) RemoveTopic(ctx context.Context, tenantID, topicID uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		courseID, err := s.authoring.DeleteTopic(ctx, tx, tenantID, topicID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTopicDeleted,
			TargetType: "topic", TargetID: topicID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"course_id": courseID.String()},
		})
	})
}

// ReorderTopics sets the order of a course's topics.
//
// The submitted list must name every topic of the course exactly once. A partial
// list would silently leave the unnamed ones wherever they were, producing an
// order the author did not ask for and cannot see.
func (s *Service) ReorderTopics(ctx context.Context, tenantID uuid.UUID, slug string, order []uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}
		if err := s.authoring.ReorderTopics(ctx, tx, tenantID, course.ID, order); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionTopicsReordered,
			TargetType: "course", TargetID: course.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"topics": len(order)},
		})
	})
}

// AddLesson appends a lesson to a topic.
func (s *Service) AddLesson(ctx context.Context, tenantID, topicID uuid.UUID, n NewLesson, author Author) (Lesson, error) {
	if err := n.validate(); err != nil {
		return Lesson{}, err
	}

	var created Lesson
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = s.authoring.CreateLesson(ctx, tx, tenantID, topicID, n)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLessonCreated,
			TargetType: "lesson", TargetID: created.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"topic_id": topicID.String(), "content_type": created.ContentType},
		})
	})
	if err != nil {
		return Lesson{}, err
	}
	return created, nil
}

// EditLesson applies a patch.
func (s *Service) EditLesson(ctx context.Context, tenantID, lessonID uuid.UUID, p LessonPatch, author Author) (Lesson, error) {
	if err := p.validate(); err != nil {
		return Lesson{}, err
	}

	var updated Lesson
	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = s.authoring.UpdateLesson(ctx, tx, tenantID, lessonID, p)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLessonUpdated,
			TargetType: "lesson", TargetID: lessonID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
		})
	})
	if err != nil {
		return Lesson{}, err
	}
	return updated, nil
}

// RemoveLesson deletes a lesson and closes the gap it leaves.
func (s *Service) RemoveLesson(ctx context.Context, tenantID, lessonID uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		topicID, err := s.authoring.DeleteLesson(ctx, tx, tenantID, lessonID)
		if err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLessonDeleted,
			TargetType: "lesson", TargetID: lessonID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"topic_id": topicID.String()},
		})
	})
}

// ReorderLessons sets the order of a topic's lessons.
func (s *Service) ReorderLessons(ctx context.Context, tenantID, topicID uuid.UUID, order []uuid.UUID, author Author) error {
	return s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.authoring.ReorderLessons(ctx, tx, tenantID, topicID, order); err != nil {
			return err
		}
		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionLessonsReordered,
			TargetType: "topic", TargetID: topicID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"lessons": len(order)},
		})
	})
}

// Publish makes a course visible to students.
//
// An empty course cannot be published. Nothing enforces that in the schema, and
// nothing should: it is a rule about what is worth selling, not about what the
// data can represent.
func (s *Service) Publish(ctx context.Context, tenantID uuid.UUID, slug string, author Author) (Course, error) {
	var published Course

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}
		if course.Status == StatusPublished {
			return ErrAlreadyPublished
		}

		lessons, err := s.authoring.CountLessons(ctx, tx, tenantID, course.ID)
		if err != nil {
			return err
		}
		if lessons == 0 {
			return ErrEmptyCourse
		}

		published, err = s.authoring.SetCourseStatus(ctx, tx, tenantID, course.ID, StatusPublished)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCoursePublished,
			TargetType: "course", TargetID: course.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"slug": slug, "lessons": lessons},
		})
	})
	if err != nil {
		return Course{}, err
	}
	return published, nil
}

// Unpublish returns a course to draft. Students lose access; enrolments are not
// touched, because unpublishing is an editorial act, not a refund.
func (s *Service) Unpublish(ctx context.Context, tenantID uuid.UUID, slug string, author Author) (Course, error) {
	var drafted Course

	err := s.db.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		course, err := s.repo.CourseBySlug(ctx, tx, tenantID, slug, true)
		if err != nil {
			return err
		}

		drafted, err = s.authoring.SetCourseStatus(ctx, tx, tenantID, course.ID, StatusDraft)
		if err != nil {
			return err
		}

		return s.audit.Record(ctx, tx, tenantID, AuditEntry{
			ActorID: &author.UserID, Action: ActionCourseUnpublished,
			TargetType: "course", TargetID: course.ID.String(),
			IP: author.IP, UserAgent: author.UserAgent,
			Metadata: map[string]any{"slug": slug},
		})
	})
	if err != nil {
		return Course{}, err
	}
	return drafted, nil
}

// validate checks a new lesson's shape. A video lesson without a video is a
// lesson that renders as an empty box.
func (n NewLesson) validate() error {
	if n.ContentType == "video" && n.VideoSource == VideoNone {
		return fmt.Errorf("%w: a video lesson needs a video source", ErrInvalidLesson)
	}
	if n.VideoSource != VideoNone && n.VideoURL == "" {
		return fmt.Errorf("%w: a video source needs a URL", ErrInvalidLesson)
	}
	if n.DurationSeconds < 0 {
		return fmt.Errorf("%w: duration cannot be negative", ErrInvalidLesson)
	}
	return nil
}

func (p LessonPatch) validate() error {
	if p.DurationSeconds != nil && *p.DurationSeconds < 0 {
		return fmt.Errorf("%w: duration cannot be negative", ErrInvalidLesson)
	}
	if p.VideoSource != nil && *p.VideoSource != VideoNone && p.VideoURL != nil && *p.VideoURL == "" {
		return fmt.Errorf("%w: a video source needs a URL", ErrInvalidLesson)
	}
	return nil
}
