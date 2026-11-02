package syncer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// batchingDest gives a connection MULTIAPPEND by appending each message in turn,
// because go-imap's test server has no such command. What is under test here is
// what the syncer does with a batch, not how one travels.
type batchingDest struct {
	imapx.Conn
	batches *atomic.Int32
	largest *atomic.Int32

	// refuse fails any batch holding this subject, as a server would.
	refuse string
	// lose fails this many batches after they were stored, as a connection
	// dropped before the response would.
	lose *atomic.Int32
	// silent leaves the UIDs out of the response.
	silent bool
}

func (d batchingDest) CanAppendBatch() bool { return true }

func (d batchingDest) AppendBatch(ctx context.Context, mailbox string, msgs []imapx.AppendMessage) ([]imapx.AppendResult, error) {
	d.batches.Add(1)
	for n := d.largest.Load(); int32(len(msgs)) > n && !d.largest.CompareAndSwap(n, int32(len(msgs))); {
		n = d.largest.Load()
	}

	bodies := make([][]byte, len(msgs))
	for i, msg := range msgs {
		body, err := io.ReadAll(msg.Body)
		if err != nil {
			return nil, err
		}
		bodies[i] = body
		if d.refuse != "" && strings.Contains(string(body), d.refuse) {
			return nil, &imap.Error{Type: imap.StatusResponseTypeNo, Text: "one of these is unwelcome"}
		}
	}

	results := make([]imapx.AppendResult, len(msgs))
	for i, msg := range msgs {
		msg.Body = bytes.NewReader(bodies[i])
		res, err := d.Conn.Append(ctx, mailbox, msg)
		if err != nil {
			return nil, err
		}
		results[i] = res
	}

	if d.lose != nil && d.lose.Add(-1) >= 0 {
		return nil, fmt.Errorf("connection dropped: %w", imapx.ErrConnectionBroken)
	}
	if d.silent {
		return make([]imapx.AppendResult, len(msgs)), nil
	}
	return results, nil
}

// TestAFolderTravelsInBatches is the reason for all of it. Sixteen workers would
// split the queue into sixteen small batches if each drew from it directly.
func TestAFolderTravelsInBatches(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 120)

	dest := batchingDest{batches: &atomic.Int32{}, largest: &atomic.Int32{}}
	report, err := syncFlaky(t, h, 4, 8, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		dest := dest
		dest.Conn = c
		return dest
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if copied, _, failed := report.Totals(); copied != len(want) || failed != 0 {
		t.Errorf("copied %d and failed %d, want %d copied and none failed", copied, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)

	if got := dest.batches.Load(); got == 0 || int(got) > len(want)/10 {
		t.Errorf("%d messages took %d batches; the point is to send far fewer commands", len(want), got)
	}
	if got := dest.largest.Load(); got < 20 {
		t.Errorf("the largest batch held %d messages", got)
	}
}

// TestARefusedBatchFallsBackToSingleMessages is the cost of atomicity: one
// message the server will not take must not take its neighbours down with it.
func TestARefusedBatchFallsBackToSingleMessages(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 40)

	tries := &atomic.Int32{}
	dest := batchingDest{batches: &atomic.Int32{}, largest: &atomic.Int32{}, refuse: want[7]}
	report, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		dest := dest
		dest.Conn = refusingDest{Conn: c, subject: want[7], tries: tries}
		return dest
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if copied, _, failed := report.Totals(); copied != len(want)-1 || failed != 1 {
		t.Errorf("copied %d and failed %d, want %d copied and 1 failed", copied, failed, len(want)-1)
	}
	assertExactly(t, h.dst, "INBOX", slices.Concat(want[:7], want[8:]))
}

// TestABatchThatLandedBeforeTheConnectionDiedIsNotSentAgain is the same promise
// the single path makes, with more at stake: a whole batch of duplicates.
func TestABatchThatLandedBeforeTheConnectionDiedIsNotSentAgain(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 40)

	lose := &atomic.Int32{}
	lose.Store(1)
	dest := batchingDest{batches: &atomic.Int32{}, largest: &atomic.Int32{}, lose: lose}
	report, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		dest := dest
		dest.Conn = c
		return dest
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if copied, _, failed := report.Totals(); copied != len(want) || failed != 0 {
		t.Errorf("copied %d and failed %d, want %d copied and none failed", copied, failed, len(want))
	}
	assertExactly(t, h.dst, "INBOX", want)
}

// TestABatchWithoutUIDsIsStillRecorded covers a server that stores a batch and
// does not say where. A second run finding nothing to do proves every message
// was recorded rather than merely delivered.
func TestABatchWithoutUIDsIsStillRecorded(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	want := fill(t, h.src, "INBOX", 30)

	dest := batchingDest{batches: &atomic.Int32{}, largest: &atomic.Int32{}, silent: true}
	wrap := func(c imapx.Conn) imapx.Conn {
		dest := dest
		dest.Conn = c
		return dest
	}
	if _, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, wrap); err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	assertExactly(t, h.dst, "INBOX", want)

	report, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, wrap)
	if err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if copied, _, _ := report.Totals(); copied != 0 {
		t.Errorf("the second run copied %d messages, want none", copied)
	}
	assertExactly(t, h.dst, "INBOX", want)
}
