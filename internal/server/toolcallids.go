package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"regexp"
)

// indexToolCallID matches a tool call id whose value is only a per-response
// index, such as "id":"call_0". Bedrock's OpenAI-compatible chat endpoint
// numbers tool calls from zero in every response, so the same id recurs on
// each turn of a conversation. Clients that treat tool call ids as unique
// across a conversation, such as agents guarding against replayed model
// output, then reject the second turn's call.
//
// The pattern matches only structural JSON: the same text inside a JSON string
// value (tool arguments, message content) is escaped (\"id\":\"call_0\") and
// never matches.
var indexToolCallID = regexp.MustCompile(`("id"\s*:\s*")(call_[0-9]+)(")`)

// maxRewriteLine bounds the bytes buffered while waiting for a line ending. A
// longer line is relayed unmodified rather than held in memory.
const maxRewriteLine = 4 << 20

// toolCallIDRewriter relays upstream bytes line by line, giving index-style
// tool call ids a suffix that is fixed for one response. Every other byte is
// unchanged, and each completed line is released as soon as it arrives, so
// streaming stays incremental. Streamed chunks repeat an id only in the chunk
// that starts its call, and the fixed suffix keeps any repeat consistent.
type toolCallIDRewriter struct {
	src     io.Reader
	suffix  []byte
	pending bytes.Buffer
	out     bytes.Buffer
	srcErr  error
	buf     []byte
}

func newToolCallIDRewriter(src io.Reader, suffix string) *toolCallIDRewriter {
	return &toolCallIDRewriter{
		src:    src,
		suffix: []byte("${1}${2}_" + suffix + "${3}"),
		buf:    make([]byte, 32*1024),
	}
}

func (r *toolCallIDRewriter) Read(p []byte) (int, error) {
	for r.out.Len() == 0 {
		if r.srcErr != nil {
			if r.pending.Len() == 0 {
				return 0, r.srcErr
			}
			r.out.Write(r.rewrite(r.pending.Bytes()))
			r.pending.Reset()
			break
		}
		n, err := r.src.Read(r.buf)
		r.pending.Write(r.buf[:n])
		r.releaseCompleteLines()
		if r.pending.Len() > maxRewriteLine {
			r.out.Write(r.pending.Bytes())
			r.pending.Reset()
		}
		r.srcErr = err
	}
	return r.out.Read(p)
}

func (r *toolCallIDRewriter) releaseCompleteLines() {
	data := r.pending.Bytes()
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		return
	}
	r.out.Write(r.rewrite(data[:end+1]))
	rest := append([]byte(nil), data[end+1:]...)
	r.pending.Reset()
	r.pending.Write(rest)
}

func (r *toolCallIDRewriter) rewrite(b []byte) []byte {
	return indexToolCallID.ReplaceAll(b, r.suffix)
}

// randomToolCallSuffix returns a short random suffix, unique per response for
// practical purposes.
func randomToolCallSuffix() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
