package mailer

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func readRecords(t *testing.T, path string) []record {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []record
	scanner := bufio.NewScanner(f)

	// A rendered HTML body easily exceeds Scanner's 64 KiB default, and the
	// concurrency test writes deliberately large ones.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		var r record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			t.Fatalf("line is not one JSON object: %q: %v", scanner.Text(), err)
		}
		out = append(out, r)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}

func TestFileAppendsOneJSONObjectPerMessage(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "mail.jsonl")
	m, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	for _, subject := range []string{"first", "second"} {
		if err := m.Send(t.Context(), Message{
			To: "ada@example.test", Subject: subject, Text: "body", HTML: "<p>body</p>",
		}); err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	got := readRecords(t, path)
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	if got[0].Subject != "first" || got[1].Subject != "second" {
		t.Errorf("messages out of order: %q, %q", got[0].Subject, got[1].Subject)
	}
	if got[0].To != "ada@example.test" || got[0].Text != "body" {
		t.Errorf("record does not carry the message: %+v", got[0])
	}
	if got[0].SentAt.IsZero() {
		t.Error("record has no timestamp")
	}
}

// A worker runs jobs concurrently, so a reader must find one whole record per
// line however many senders were writing. This passes without the mutex — a
// single write() under O_APPEND is not split — and is here to keep it that way,
// not to justify the lock.
func TestFileIsSafeForConcurrentSenders(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "mail.jsonl")
	m, err := NewFile(path)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}

	// A long body makes an interleaved write obvious: a short one may fit in a
	// single atomic append and hide the bug.
	body := string(make([]byte, 64*1024))

	const senders = 8
	var wg sync.WaitGroup
	wg.Add(senders)

	for i := range senders {
		go func() {
			defer wg.Done()
			if err := m.Send(t.Context(), Message{
				To: "ada@example.test", Subject: "concurrent", Text: body, HTML: body,
			}); err != nil {
				t.Errorf("sender %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	if got := readRecords(t, path); len(got) != senders {
		t.Errorf("records = %d, want %d — writes interleaved", len(got), senders)
	}
}

func TestNewFileRefusesAnEmptyPath(t *testing.T) {
	t.Parallel()

	if _, err := NewFile(""); err == nil {
		t.Error("NewFile accepted an empty path")
	}
}

func TestNewFileFailsAtConstructionForAnUnwritablePath(t *testing.T) {
	t.Parallel()

	// A directory that does not exist. Failing here beats failing inside a
	// retrying job, where the only observer is the queue.
	if _, err := NewFile(filepath.Join(t.TempDir(), "no-such-dir", "mail.jsonl")); err == nil {
		t.Error("NewFile accepted a path it cannot write")
	}
}
