package syncer_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hilli/imapsync-go/internal/imapx"
	"github.com/hilli/imapsync-go/internal/syncer"
)

// namespaced reports a personal namespace, which go-imap's test server has no
// way to configure.
type namespaced struct {
	imapx.Conn
	prefix string
	err    error
}

func (n namespaced) Namespaces(context.Context) (imapx.Namespaces, error) {
	return imapx.Namespaces{PersonalPrefix: n.prefix, Delim: "/", Supported: true}, n.err
}

func inNamespace(prefix string) func(imapx.Conn) imapx.Conn {
	return func(c imapx.Conn) imapx.Conn {
		return namespaced{Conn: c, prefix: prefix}
	}
}

// TestNewFoldersGoInsideTheDestinationNamespace is the failure that started
// this: planning with the source's own names asks a prefixed Dovecot to create
// folders outside the only namespace it has.
func TestNewFoldersGoInsideTheDestinationNamespace(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Work")
	want := fill(t, h.src, "Work", 5)

	if _, err := syncFlaky(t, h, 2, 2, syncer.Options{}, nil, inNamespace("INBOX/")); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertExactly(t, h.dst, "INBOX/Work", want)
}

func TestTheSourceNamespaceIsNotCopiedAlong(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps(), "Mail/Work")
	want := fill(t, h.src, "Mail/Work", 5)

	if _, err := syncFlaky(t, h, 2, 2, syncer.Options{}, inNamespace("Mail/"), nil); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	assertExactly(t, h.dst, "Work", want)
}

// TestAnUnansweredNamespaceStopsThePlan refuses to guess, because a wrong guess
// is folders created in the wrong place.
func TestAnUnansweredNamespaceStopsThePlan(t *testing.T) {
	t.Parallel()

	h := newHarness(t, rev1Caps())
	fill(t, h.src, "INBOX", 3)

	refused := errors.New("no namespace for you")
	_, err := syncFlaky(t, h, 1, 1, syncer.Options{}, nil, func(c imapx.Conn) imapx.Conn {
		return namespaced{Conn: c, err: refused}
	})
	if !errors.Is(err, refused) {
		t.Errorf("Run() error = %v, want the namespace failure", err)
	}
}
