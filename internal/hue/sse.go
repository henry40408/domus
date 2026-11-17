package hue

import (
	"bytes"
	"strings"
)

// sseParser is an incremental server-sent-events parser that returns the data
// of each event.
type sseParser struct {
	buf []byte
}

// push adds a chunk and returns the data payload of every event it completed.
// Carriage returns are dropped up front, so CRLF and LF streams read the same
// and an event boundary may be split across chunks.
func (p *sseParser) push(chunk []byte) []string {
	p.buf = append(p.buf, bytes.ReplaceAll(chunk, []byte("\r"), nil)...)
	var payloads []string
	for {
		end := bytes.Index(p.buf, []byte("\n\n"))
		if end < 0 {
			return payloads
		}
		block := strings.ToValidUTF8(string(p.buf[:end]), "�")
		p.buf = p.buf[end+2:]
		if data := dataLines(block); len(data) > 0 {
			payloads = append(payloads, strings.Join(data, "\n"))
		}
	}
}

// dataLines are the values of the "data:" lines of one event block.
func dataLines(block string) []string {
	var data []string
	for line := range strings.SplitSeq(block, "\n") {
		value, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = append(data, strings.TrimPrefix(value, " "))
	}
	return data
}
