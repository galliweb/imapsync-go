package imapx

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/emersion/go-imap/v2"
)

// batchResponseTimeout bounds the wait for a batch's tagged response. It is
// go-imap's own allowance for a literal, because a batch is one long literal in
// everything but syntax.
const batchResponseTimeout = 5 * time.Minute

// batchWriteBuffer is how much of a batch is gathered before a write reaches
// the socket.
const batchWriteBuffer = 64 << 10

// BatchAppender is a connection that can store several messages in one command.
//
// It is separate from Conn because most connections have no such thing: a
// server has to offer MULTIAPPEND, and the local store has nothing to gain.
type BatchAppender interface {
	// CanAppendBatch reports whether AppendBatch can be used on this connection.
	CanAppendBatch() bool

	// AppendBatch stores msgs in mailbox with a single APPEND command (RFC 3502)
	// and reports where each landed, in order. A result is unassigned when the
	// server did not say.
	//
	// The command is atomic: an error from the server means none of the messages
	// were stored. An error wrapping ErrConnectionBroken means nobody can say
	// whether they were.
	AppendBatch(ctx context.Context, mailbox string, msgs []AppendMessage) ([]AppendResult, error)
}

var _ BatchAppender = (*conn)(nil)

// CanAppendBatch needs LITERAL+ as well as MULTIAPPEND. Without it every message
// would wait a round trip for permission to be sent, and a refusal partway
// through would have to be told apart from that permission.
func (c *conn) CanAppendBatch() bool {
	caps := c.Caps()
	return c.tap != nil && caps.MultiAppend && caps.LiteralPlus
}

// AppendBatch implements BatchAppender.
//
// One command with many messages is not merely tidier than many commands. A
// server may spend most of an APPEND committing it, and cyon was measured doing
// exactly that: about a second per command whether it carried one small message
// or fifty, which no number of connections could get around.
//
// It bypasses go-imap, which has no such command and whose parser treats the
// UID set in the answer as a protocol error that ends the connection. A Conn is
// never used concurrently, so nothing of go-imap's is in flight meanwhile.
func (c *conn) AppendBatch(ctx context.Context, mailbox string, msgs []AppendMessage) ([]AppendResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("appending to %q: %w", mailbox, err)
	}
	if !c.CanAppendBatch() {
		return nil, fmt.Errorf("appending to %q: this connection cannot batch", mailbox)
	}
	name := encodeMailbox(mailbox, c.Caps().IMAP4rev2)

	tag, answer := c.tap.watch()
	defer c.tap.unwatch()

	if err := c.tap.sendAppend(tag, name, msgs); err != nil {
		// However much of the batch went out, the server is now reading the
		// remainder of a literal that is never coming.
		_ = c.c.Close()
		return nil, fmt.Errorf("appending %d messages to %q: %v: %w", len(msgs), mailbox, err, ErrConnectionBroken)
	}

	timeout := time.NewTimer(batchResponseTimeout)
	defer timeout.Stop()

	select {
	case line := <-answer:
		results, err := appendOutcome(line, tag, len(msgs))
		if err != nil {
			return nil, fmt.Errorf("appending %d messages to %q: %w", len(msgs), mailbox, err)
		}
		return results, nil

	case <-c.c.Closed():
		return nil, fmt.Errorf("appending %d messages to %q: connection lost: %w", len(msgs), mailbox, ErrConnectionBroken)

	case <-timeout.C:
		_ = c.c.Close()
		return nil, fmt.Errorf("appending %d messages to %q: no response after %s: %w",
			len(msgs), mailbox, batchResponseTimeout, ErrConnectionBroken)
	}
}

// tap sits between go-imap and the socket, so that one command can be sent and
// answered without go-imap knowing about it.
//
// It is the smallest way to have a command the library lacks while keeping
// everything else it does: the pool, authentication and every other command
// stay on the same connection. The read side runs on go-imap's own read loop,
// which is always blocked in Read between commands. While a response is being
// watched for, that one line is lifted out of the stream and everything around
// it is passed on, so a mailbox update arriving mid-batch still reaches go-imap.
type tap struct {
	net.Conn
	trace io.Writer

	seq uint64

	mu      sync.Mutex
	tag     string
	answer  chan string
	partial []byte
	held    []byte
}

func newTap(raw net.Conn, trace io.Writer) *tap {
	return &tap{Conn: raw, trace: trace}
}

// watch reserves a tag and starts looking for its response. It has to come
// before the command is sent, or the response could arrive first.
func (t *tap) watch() (string, <-chan string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	t.tag = "M" + strconv.FormatUint(t.seq, 10)
	t.answer = make(chan string, 1)
	return t.tag, t.answer
}

// unwatch stops looking, and hands back anything held while looking.
func (t *tap) unwatch() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tag = ""
	t.held = append(t.held, t.partial...)
	t.partial = nil
}

// Read serves go-imap's read loop.
func (t *tap) Read(p []byte) (int, error) {
	for {
		if n := t.drain(p); n > 0 {
			return n, nil
		}
		n, err := t.Conn.Read(p)
		kept := t.sift(p[:n])
		n = copy(p, kept)
		t.hold(kept[n:])
		if n > 0 || err != nil {
			return n, err
		}
	}
}

func (t *tap) drain(p []byte) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := copy(p, t.held)
	t.held = t.held[n:]
	return n
}

func (t *tap) hold(data []byte) {
	if len(data) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.held = append(t.held, data...)
}

// sift returns data without the watched response. A line that has not arrived
// in full is kept back, because it cannot yet be told apart from that response.
func (t *tap) sift(data []byte) []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.tag == "" {
		return data
	}

	data = append(t.partial, data...)
	t.partial = nil

	prefix := []byte(t.tag + " ")
	var kept []byte
	for t.tag != "" {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			t.partial = append(t.partial, data...)
			return kept
		}
		line := data[:end+1]
		data = data[end+1:]
		if !bytes.HasPrefix(line, prefix) {
			kept = append(kept, line...)
			continue
		}
		text := string(bytes.TrimRight(line, "\r\n"))
		t.note("S: " + text)
		t.answer <- text
		t.tag = ""
	}
	return append(kept, data...)
}

// sendAppend writes the whole command. LITERAL+ means nothing has to be waited
// for along the way.
func (t *tap) sendAppend(tag, mailbox string, msgs []AppendMessage) error {
	t.note(fmt.Sprintf("C: %s APPEND %s (%d messages)", tag, mailbox, len(msgs)))

	if err := t.SetWriteDeadline(time.Now().Add(batchResponseTimeout)); err != nil {
		return err
	}
	w := bufio.NewWriterSize(t.Conn, batchWriteBuffer)
	fmt.Fprintf(w, "%s APPEND %s", tag, mailbox)
	for _, msg := range msgs {
		w.WriteByte(' ')
		if len(msg.Flags) > 0 {
			w.WriteString("(" + strings.Join(msg.Flags, " ") + ") ")
		}
		if !msg.InternalDate.IsZero() {
			w.WriteString(`"` + msg.InternalDate.Format(appendDateLayout) + `" `)
		}
		fmt.Fprintf(w, "{%d+}\r\n", msg.Size)

		written, err := io.Copy(w, msg.Body)
		if err != nil {
			return err
		}
		if written != msg.Size {
			return fmt.Errorf("declared %d bytes but sent %d", msg.Size, written)
		}
	}
	w.WriteString("\r\n")
	return w.Flush()
}

func (t *tap) note(line string) {
	if t.trace != nil {
		fmt.Fprintf(t.trace, "%s\r\n", line)
	}
}

// appendDateLayout is RFC 3501's date-time.
const appendDateLayout = "_2-Jan-2006 15:04:05 -0700"

// appendUIDCode is the UIDPLUS answer to a MULTIAPPEND: the mailbox's
// UIDVALIDITY and a UID set covering every message stored.
var appendUIDCode = regexp.MustCompile(`(?i)\[APPENDUID (\d+) ([0-9:,]+)\]`)

// appendOutcome reads the tagged response to a batch.
func appendOutcome(line, tag string, count int) ([]AppendResult, error) {
	status, text, _ := strings.Cut(strings.TrimPrefix(line, tag+" "), " ")

	var kind imap.StatusResponseType
	switch strings.ToUpper(status) {
	case "OK":
		return assignedUIDs(text, count), nil
	case "NO":
		kind = imap.StatusResponseTypeNo
	case "BAD":
		kind = imap.StatusResponseTypeBad
	default:
		return nil, fmt.Errorf("unexpected response %q: %w", line, ErrConnectionBroken)
	}

	// The same error go-imap would have built, so that retry.Classify reads a
	// refused batch exactly as it reads a refused message.
	out := &imap.Error{Type: kind, Text: text}
	if rest, ok := strings.CutPrefix(text, "["); ok {
		if code, after, ok := strings.Cut(rest, "]"); ok {
			word, _, _ := strings.Cut(code, " ")
			out.Code = imap.ResponseCode(strings.ToUpper(word))
			out.Text = strings.TrimSpace(after)
		}
	}
	return nil, out
}

// assignedUIDs matches the UIDs in a response to the messages sent, which RFC
// 4315 says are in the same order. A response that does not account for every
// message assigns none: finding them again costs a search, and misfiling one
// costs a message.
func assignedUIDs(text string, count int) []AppendResult {
	out := make([]AppendResult, count)

	m := appendUIDCode.FindStringSubmatch(text)
	if m == nil {
		return out
	}
	validity, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil {
		return out
	}
	uids, ok := expandUIDSet(m[2], count)
	if !ok {
		return out
	}
	for i, uid := range uids {
		out[i] = AppendResult{UID: uid, UIDValidity: uint32(validity)}
	}
	return out
}

// expandUIDSet lists a UID set in the order given, and reports whether it holds
// exactly count UIDs.
func expandUIDSet(set string, count int) ([]uint32, bool) {
	uids := make([]uint32, 0, count)
	for _, part := range strings.Split(set, ",") {
		low, high, isRange := strings.Cut(part, ":")
		if !isRange {
			high = low
		}
		first, err := strconv.ParseUint(low, 10, 32)
		if err != nil {
			return nil, false
		}
		last, err := strconv.ParseUint(high, 10, 32)
		if err != nil || first == 0 || last < first || last-first >= uint64(count) {
			return nil, false
		}
		for uid := first; uid <= last; uid++ {
			uids = append(uids, uint32(uid))
		}
		if len(uids) > count {
			return nil, false
		}
	}
	return uids, len(uids) == count
}

// mailboxBase64 is the alphabet of modified UTF-7, RFC 3501 section 5.1.3.
var mailboxBase64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").
	WithPadding(base64.NoPadding)

// encodeMailbox renders a mailbox name as a quoted string, the way go-imap would
// have. An IMAP4rev2 server takes UTF-8 and any other takes modified UTF-7.
func encodeMailbox(name string, utf8Names bool) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	encoded := strings.ReplaceAll(name, "&", "&-")
	if !utf8Names {
		encoded = modifiedUTF7(name)
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(encoded) + `"`
}

func modifiedUTF7(name string) string {
	var (
		out strings.Builder
		run []uint16
	)
	flush := func() {
		if len(run) == 0 {
			return
		}
		raw := make([]byte, 0, len(run)*2)
		for _, unit := range run {
			raw = append(raw, byte(unit>>8), byte(unit))
		}
		out.WriteString("&" + mailboxBase64.EncodeToString(raw) + "-")
		run = run[:0]
	}

	for _, r := range name {
		if r < 0x20 || r > 0x7e {
			run = utf16.AppendRune(run, r)
			continue
		}
		flush()
		if r == '&' {
			out.WriteString("&-")
		} else {
			out.WriteRune(r)
		}
	}
	flush()
	return out.String()
}
