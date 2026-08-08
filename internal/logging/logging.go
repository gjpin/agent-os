package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"time"

	"github.com/gjpin/agent-os/internal/model"
)

type Logger struct {
	Out    io.Writer
	Err    io.Writer
	Format model.LogFormat
}

func (l Logger) Event(kind, message string, fields map[string]any) {
	if l.Out == nil {
		l.Out = io.Discard
	}
	if l.Format == model.LogJSON {
		value := map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "event": kind, "message": message}
		for key, item := range fields {
			value[key] = Redact(key, item)
		}
		data, err := json.Marshal(value)
		if err == nil {
			fmt.Fprintln(l.Out, string(data))
		}
		return
	}
	fmt.Fprintln(l.Out, message)
}

func (l Logger) Diagnostic(message string) {
	out := l.Err
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintln(out, message)
}

func Redact(key string, value any) any {
	switch key {
	case "token", "access_token", "password", "passphrase", "private_key", "secret", "authorization", "auth_response":
		return "<redacted>"
	default:
		return value
	}
}

type RedactingWriter struct {
	Writer io.Writer
}

var secretText = regexp.MustCompile(`(?i)((?:pairing|access[_-]?token|token|passphrase|password|secret)[=:][[:space:]]*)[^[:space:]]+`)
var secretURL = regexp.MustCompile(`(?i)https?://[^[:space:]]*(?:pair|token|auth|secret)[^[:space:]]*`)

func (w RedactingWriter) Write(data []byte) (int, error) {
	if w.Writer == nil {
		return len(data), nil
	}
	clean := secretText.ReplaceAllString(string(data), `${1}<redacted>`)
	clean = secretURL.ReplaceAllString(clean, "<redacted-url>")
	_, err := io.WriteString(w.Writer, clean)
	if err != nil {
		return 0, err
	}
	return len(data), nil
}
