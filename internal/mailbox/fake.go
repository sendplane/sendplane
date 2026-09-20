package mailbox

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fake is an in-memory Client for testing consumers. It is safe for concurrent
// use, so two pollers can race for the same mailbox in a test exactly as two
// replicas would.
//
// It is not a protocol simulator: it models the one guarantee the interface
// makes, which is that Fetch returns messages that have not been acked yet.
type Fake struct {
	mu sync.Mutex
	// msgs is the mailbox content, in arrival order.
	msgs []Message
	// acked records what Ack did, in call order, so a test can assert the
	// after-process policy without inspecting a server.
	acked []Ack
	// seen is the set of acked IDs; Fetch skips them. Kept messages stay in
	// msgs, which is what an IMAP \Seen flag models.
	seen map[string]bool

	// FetchErr and AckErr, when set, are returned instead of doing the work.
	FetchErr error
	AckErr   error
	// Closed counts Close calls.
	Closed int
	// Fetches counts Fetch calls.
	Fetches int
}

// Ack is one recorded acknowledgement.
type Ack struct {
	IDs    []string
	Action Action
}

var _ Client = (*Fake)(nil)

// NewFake returns a fake mailbox holding raws, with IDs "1", "2", ...
func NewFake(raws ...[]byte) *Fake {
	f := &Fake{seen: map[string]bool{}}
	for _, raw := range raws {
		f.Add(raw)
	}
	return f
}

// Add appends one message and returns its ID.
func (f *Fake) Add(raw []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	id := strconv.Itoa(len(f.msgs) + 1)
	f.msgs = append(f.msgs, Message{
		ID:       id,
		Raw:      raw,
		Received: time.Unix(int64(len(f.msgs)), 0).UTC(),
	})
	return id
}

func (f *Fake) Fetch(_ context.Context, max int) ([]Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Fetches++
	if f.FetchErr != nil {
		return nil, f.FetchErr
	}
	out := make([]Message, 0, len(f.msgs))
	for _, m := range f.msgs {
		if f.seen[m.ID] {
			continue
		}
		out = append(out, m)
		if max > 0 && len(out) == max {
			break
		}
	}
	return out, nil
}

func (f *Fake) Ack(_ context.Context, ids []string, action Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AckErr != nil {
		return f.AckErr
	}
	if len(ids) == 0 {
		return nil
	}
	f.acked = append(f.acked, Ack{IDs: append([]string(nil), ids...), Action: action})
	for _, id := range ids {
		f.seen[id] = true
	}
	if action == ActionDelete {
		kept := f.msgs[:0]
		for _, m := range f.msgs {
			if !containsID(ids, m.ID) {
				kept = append(kept, m)
			}
		}
		f.msgs = kept
	}
	return nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed++
	return nil
}

// Acks returns the recorded acknowledgements.
func (f *Fake) Acks() []Ack {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Ack(nil), f.acked...)
}

// AckedIDs returns every acked ID, sorted.
func (f *Fake) AckedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.seen))
	for id := range f.seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Remaining is how many messages are still in the mailbox.
func (f *Fake) Remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
