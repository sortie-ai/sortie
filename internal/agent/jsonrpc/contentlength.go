package jsonrpc

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// WriteContentLengthFrame writes payload as one Content-Length framed message,
// the framing language-server style peers expect instead of newline-delimited
// JSON.
func WriteContentLengthFrame(w io.Writer, payload []byte) error {
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadContentLengthFrame reads one Content-Length framed message and returns
// its body. A declared length above limit is refused before any of the body is
// read, and a header block without a Content-Length is an error.
func ReadContentLengthFrame(r *bufio.Reader, limit int) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			continue
		}
		n, convErr := strconv.Atoi(strings.TrimSpace(value))
		if convErr != nil {
			return nil, fmt.Errorf("invalid Content-Length header %q: %w", value, convErr)
		}
		if n > limit {
			return nil, fmt.Errorf("content-length %d exceeds %d byte limit", n, limit)
		}
		length = n
	}
	if length < 0 {
		return nil, fmt.Errorf("message carries no Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}
