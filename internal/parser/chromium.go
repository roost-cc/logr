package parser

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/roost-cc/logr/internal/db"
)

// parseChromium handles Chromium/Chrome stderr log lines:
//
//	[3009052:3009058:0418/054625.874179:ERROR:net/socket/ssl_client_socket_impl.cc(924)] handshake failed; ...
//	 ^pid    ^tid    ^MMDD ^HHMMss.us  ^level ^file(line)                               ^message
var chromiumRe = regexp.MustCompile(
	`^\[(\d+):(\d+):(\d{4})/(\d{6}\.\d+):(VERBOSE\d*|INFO|WARNING|ERROR|FATAL):([^\]]+)\]\s*(.*)$`,
)

// chromiumFileRe splits "path/to/file.cc(123)" into file and line.
var chromiumFileRe = regexp.MustCompile(`^(.+?)\((\d+)\)$`)

func parseChromium(source, line, raw string) (db.Record, bool) {
	m := chromiumRe.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return db.Record{}, false
	}

	pid, _ := strconv.ParseInt(m[1], 10, 64)
	tid, _ := strconv.ParseInt(m[2], 10, 64)
	// m[3] = MMDD, m[4] = HHMMSS.us — parse into a wall-clock time
	logTime := tryParseChromiumTime(m[3], m[4])

	cdpLevel := m[5]
	fileRef := m[6] // e.g. "net/socket/ssl_client_socket_impl.cc(924)"
	msg := strings.TrimSpace(m[7])

	data := map[string]any{
		"pid":  pid,
		"tid":  tid,
		"_raw": raw,
	}

	if fm := chromiumFileRe.FindStringSubmatch(fileRef); fm != nil {
		data["file"] = fm[1]
		if ln, err := strconv.ParseInt(fm[2], 10, 64); err == nil {
			data["line"] = ln
		}
	} else {
		data["file"] = fileRef
	}

	if msg == "" {
		msg = fmt.Sprintf("%s in %s", cdpLevel, fileRef)
	}

	return db.Record{
		LogTime: logTime,
		Source:  source,
		Level:   chromiumLevel(cdpLevel),
		Message: msg,
		Data:    data,
	}, true
}

func chromiumLevel(s string) string {
	switch {
	case strings.HasPrefix(s, "VERBOSE"):
		return "DEBUG"
	case s == "INFO":
		return "INFO"
	case s == "WARNING":
		return "WARN"
	case s == "ERROR":
		return "ERROR"
	case s == "FATAL":
		return "FATAL"
	}
	return "INFO"
}

// tryParseChromiumTime parses MMDD + HHMMSS.us into a *time.Time.
// Example: mmdd="0418", hhmmssus="054625.874179"
func tryParseChromiumTime(mmdd, hhmmssus string) *time.Time {
	if len(mmdd) != 4 || len(hhmmssus) < 6 {
		return nil
	}
	year := time.Now().Year()
	s := fmt.Sprintf("%d%s %s", year, mmdd, hhmmssus)
	t, err := time.ParseInLocation("20060102 150405.999999", s, time.Local)
	if err != nil {
		return nil
	}
	return &t
}
