package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// File appends each message to a file, one JSON object per line, instead of
// sending it.
//
// It exists so that an automated test can read the link a message carries. Every
// message this system sends contains a single-use credential, and there is no
// other way for a test to accept an invitation or follow a reset link without
// standing up an SMTP server — which is why removing the invitation token from
// the API response made student provisioning untestable until this existed.
//
// Development only. It writes credentials to disk in plaintext, so config
// refuses it anywhere else.
type File struct {
	path string

	// A worker runs jobs concurrently. O_APPEND already makes each write land at
	// the end of the file, and a single write() of one line is not split, so this
	// lock is not what keeps lines whole — removing it does not break the test
	// that says so. It serialises the open/write/close as one step, which is the
	// invariant a reader tailing the file actually depends on.
	mu sync.Mutex
}

// NewFile returns a Mailer that appends messages to path, creating it if needed.
func NewFile(path string) (*File, error) {
	if path == "" {
		return nil, errors.New("mailer: file path is required")
	}

	// Fail at construction rather than on the first message, when the only
	// observer is a retrying job.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("mailer: open %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("mailer: close %s: %w", path, err)
	}

	return &File{path: path}, nil
}

// record is the on-disk shape. It is read by tests, so the field names are part
// of a contract, small as it is.
type record struct {
	To      string    `json:"to"`
	Subject string    `json:"subject"`
	Text    string    `json:"text"`
	HTML    string    `json:"html"`
	SentAt  time.Time `json:"sent_at"`
}

// Send appends m.
func (f *File) Send(ctx context.Context, m Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	line, err := json.Marshal(record{
		To: m.To, Subject: m.Subject, Text: m.Text, HTML: m.HTML, SentAt: time.Now().UTC(),
	})
	if err != nil {
		return fmt.Errorf("mailer: encode message: %w", err)
	}
	line = append(line, '\n')

	f.mu.Lock()
	defer f.mu.Unlock()

	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("mailer: open %s: %w", f.path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("mailer: append to %s: %w", f.path, err)
	}
	return nil
}
