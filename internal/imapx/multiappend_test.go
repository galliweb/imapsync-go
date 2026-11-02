package imapx_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"

	"github.com/hilli/imapsync-go/internal/imapx"
)

const batchCaps = "IMAP4rev1 LITERAL+ MULTIAPPEND UIDPLUS"

// batchCommand is an APPEND as the server received it.
type batchCommand struct {
	line     string
	literals [][]byte
}

// batchServer speaks just enough IMAP to be handed a MULTIAPPEND, because
// go-imap's own test server has no such command. answer writes the response and
// reports whether to keep the connection.
type batchServer struct {
	caps   string
	answer func(w io.Writer, tag string) bool
	got    chan batchCommand
}

func startBatchServer(t *testing.T, caps string, answer func(w io.Writer, tag string) bool) (string, <-chan batchCommand) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	s := &batchServer{caps: caps, answer: answer, got: make(chan batchCommand, 8)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return ln.Addr().String(), s.got
}

func (s *batchServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()

	r := bufio.NewReader(c)
	fmt.Fprintf(c, "* OK [CAPABILITY %s] ready\r\n", s.caps)
	for {
		cmd, err := readBatchCommand(r)
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(cmd.line, " ")
		verb, _, _ := strings.Cut(rest, " ")
		switch strings.ToUpper(verb) {
		case "APPEND":
			s.got <- cmd
			if !s.answer(c, tag) {
				return
			}
		case "CAPABILITY":
			fmt.Fprintf(c, "* CAPABILITY %s\r\n%s OK done\r\n", s.caps, tag)
		default:
			fmt.Fprintf(c, "%s OK done\r\n", tag)
		}
	}
}

var literalHead = regexp.MustCompile(`\{(\d+)\+\}$`)

func readBatchCommand(r *bufio.Reader) (batchCommand, error) {
	var cmd batchCommand
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return cmd, err
		}
		line = strings.TrimRight(line, "\r\n")

		head := literalHead.FindStringSubmatch(line)
		if head == nil {
			cmd.line += line
			return cmd, nil
		}
		cmd.line += strings.TrimSuffix(line, head[0])

		size, err := strconv.Atoi(head[1])
		if err != nil {
			return cmd, err
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return cmd, err
		}
		cmd.literals = append(cmd.literals, body)
	}
}

func batchMessages(bodies ...string) []imapx.AppendMessage {
	msgs := make([]imapx.AppendMessage, len(bodies))
	for i, body := range bodies {
		msgs[i] = imapx.AppendMessage{
			Size:         int64(len(body)),
			Flags:        []string{`\Seen`},
			InternalDate: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC),
			Body:         strings.NewReader(body),
		}
	}
	return msgs
}

func batcher(t *testing.T, conn imapx.Conn) imapx.BatchAppender {
	t.Helper()
	b, ok := conn.(imapx.BatchAppender)
	if !ok {
		t.Fatal("a dialled connection does not implement BatchAppender")
	}
	return b
}

// respond answers every APPEND with the same response, with the command's tag.
func respond(format string) func(io.Writer, string) bool {
	return func(w io.Writer, tag string) bool {
		fmt.Fprintf(w, format, tag)
		return true
	}
}

// TestAppendBatchSendsOneCommand checks what reaches the server, which is the
// whole point: one tag, every message, and a mailbox name a server can read.
func TestAppendBatchSendsOneCommand(t *testing.T) {
	t.Parallel()

	addr, got := startBatchServer(t, batchCaps, respond("%s OK [APPENDUID 7 191:193] done\r\n"))
	conn := dialMem(t, addr)

	results, err := batcher(t, conn).AppendBatch(context.Background(), "Kunden.Müller & Co", batchMessages("one", "two", "three"))
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}

	cmd := <-got
	if want := `"Kunden.M&APw-ller &- Co"`; !strings.Contains(cmd.line, want) {
		t.Errorf("command %q does not name the mailbox as %s", cmd.line, want)
	}
	if want := `(\Seen) " 2-Nov-2026 12:00:00 +0000"`; strings.Count(cmd.line, want) != 3 {
		t.Errorf("command %q should carry flags and date for each of three messages", cmd.line)
	}
	if len(cmd.literals) != 3 || !bytes.Equal(cmd.literals[2], []byte("three")) {
		t.Errorf("server received %q, want the three message bodies", cmd.literals)
	}

	for i, want := range []uint32{191, 192, 193} {
		if results[i].UID != want || results[i].UIDValidity != 7 {
			t.Errorf("result %d = %+v, want UID %d in UIDVALIDITY 7", i, results[i], want)
		}
	}
}

// TestAppendBatchLeavesTheConnectionUsable matters because the response never
// passes through go-imap: a pool hands this connection to whoever comes next.
func TestAppendBatchLeavesTheConnectionUsable(t *testing.T) {
	t.Parallel()

	addr, _ := startBatchServer(t, batchCaps, respond("%s OK [APPENDUID 7 1:2] done\r\n"))
	conn := dialMem(t, addr)

	for i := range 3 {
		if _, err := batcher(t, conn).AppendBatch(context.Background(), "INBOX", batchMessages("a", "b")); err != nil {
			t.Fatalf("AppendBatch() #%d error = %v", i, err)
		}
		if err := conn.CreateFolder(context.Background(), "After"); err != nil {
			t.Fatalf("CreateFolder() after batch #%d error = %v", i, err)
		}
	}
}

func TestAppendBatchFollowsTheUIDSet(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		response string
		want     []uint32
	}{
		{"scattered", "%s OK [APPENDUID 7 5,7:8] done\r\n", []uint32{5, 7, 8}},
		{"too few", "%s OK [APPENDUID 7 1:2] done\r\n", []uint32{0, 0, 0}},
		{"too many", "%s OK [APPENDUID 7 1:4] done\r\n", []uint32{0, 0, 0}},
		{"none at all", "%s OK done\r\n", []uint32{0, 0, 0}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			addr, _ := startBatchServer(t, batchCaps, respond(tt.response))
			results, err := batcher(t, dialMem(t, addr)).AppendBatch(context.Background(), "INBOX", batchMessages("a", "b", "c"))
			if err != nil {
				t.Fatalf("AppendBatch() error = %v", err)
			}
			for i, want := range tt.want {
				if results[i].UID != want {
					t.Errorf("result %d has UID %d, want %d", i, results[i].UID, want)
				}
				if results[i].Assigned() != (want != 0) {
					t.Errorf("result %d Assigned() = %v, want %v", i, results[i].Assigned(), want != 0)
				}
			}
		})
	}
}

// TestAppendBatchRefusalIsAnIMAPError keeps a refused batch classified the same
// way as a refused message, and the connection in service.
func TestAppendBatchRefusalIsAnIMAPError(t *testing.T) {
	t.Parallel()

	addr, _ := startBatchServer(t, batchCaps, respond("%s NO [OVERQUOTA] mailbox is full\r\n"))
	conn := dialMem(t, addr)

	_, err := batcher(t, conn).AppendBatch(context.Background(), "INBOX", batchMessages("a", "b"))

	var refused *imap.Error
	if !errors.As(err, &refused) {
		t.Fatalf("AppendBatch() error = %v, want an *imap.Error", err)
	}
	if refused.Type != imap.StatusResponseTypeNo || refused.Code != imap.ResponseCodeOverQuota || refused.Text != "mailbox is full" {
		t.Errorf("refusal = %+v", refused)
	}
	if errors.Is(err, imapx.ErrConnectionBroken) {
		t.Error("a refusal must not condemn the connection")
	}
	if err := conn.CreateFolder(context.Background(), "After"); err != nil {
		t.Errorf("CreateFolder() after a refusal error = %v", err)
	}
}

// TestAppendBatchFindsItsResponseInTheStream sends the response in pieces, after
// an update that belongs to go-imap, as a real network may.
func TestAppendBatchFindsItsResponseInTheStream(t *testing.T) {
	t.Parallel()

	addr, _ := startBatchServer(t, batchCaps, func(w io.Writer, tag string) bool {
		for _, piece := range []string{"* 3 EX", "ISTS\r\n", tag[:1], tag[1:] + " OK [APPEND", "UID 7 1:2] done\r", "\n"} {
			fmt.Fprint(w, piece)
			time.Sleep(5 * time.Millisecond)
		}
		return true
	})
	conn := dialMem(t, addr)

	results, err := batcher(t, conn).AppendBatch(context.Background(), "INBOX", batchMessages("a", "b"))
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if results[1].UID != 2 {
		t.Errorf("second result = %+v, want UID 2", results[1])
	}
	if err := conn.CreateFolder(context.Background(), "After"); err != nil {
		t.Errorf("CreateFolder() after the update error = %v", err)
	}
}

// TestAppendBatchLostConnectionIsNotAnAnswer must not hang, and must say that
// the outcome is unknown rather than that the messages failed.
func TestAppendBatchLostConnectionIsNotAnAnswer(t *testing.T) {
	t.Parallel()

	addr, _ := startBatchServer(t, batchCaps, func(io.Writer, string) bool { return false })
	conn := dialMem(t, addr)

	done := make(chan error, 1)
	go func() {
		_, err := batcher(t, conn).AppendBatch(context.Background(), "INBOX", batchMessages("a", "b"))
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, imapx.ErrConnectionBroken) {
			t.Errorf("AppendBatch() error = %v, want ErrConnectionBroken", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AppendBatch() hung waiting for a response from a closed connection")
	}
}

func TestAppendBatchRefusesACancelledContext(t *testing.T) {
	t.Parallel()

	addr, got := startBatchServer(t, batchCaps, respond("%s OK done\r\n"))
	conn := dialMem(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := batcher(t, conn).AppendBatch(ctx, "INBOX", batchMessages("a", "b")); !errors.Is(err, context.Canceled) {
		t.Errorf("AppendBatch() error = %v, want context.Canceled", err)
	}
	select {
	case <-got:
		t.Error("a cancelled batch reached the server")
	default:
	}
}

func TestCanAppendBatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		caps string
		want bool
	}{
		{batchCaps, true},
		{"IMAP4rev1 LITERAL+ UIDPLUS", false},
		{"IMAP4rev1 MULTIAPPEND UIDPLUS", false},
	}
	for _, tt := range tests {
		addr, _ := startBatchServer(t, tt.caps, respond("%s OK done\r\n"))
		if got := batcher(t, dialMem(t, addr)).CanAppendBatch(); got != tt.want {
			t.Errorf("CanAppendBatch() with %q = %v, want %v", tt.caps, got, tt.want)
		}
	}
}
