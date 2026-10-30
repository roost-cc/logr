package parser

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/roost-cc/logr/internal/db"
)

// parseLlama handles the llama.cpp server log format:
//
//	srv  log_server_r: done request: POST /v1/chat/completions 172.18.0.1 200
//	slot update_slots: id  0 | task 7253 | prompt processing progress, ...
//	slot print_timing: id  0 | task 7253 |
//	   prompt eval time =  1079.64 ms /  6520 tokens (  0.17 ms per token, 6039.03 tokens per second)
//	          eval time =  3479.78 ms /   470 tokens (  7.40 ms per token,  135.07 tokens per second)
//	         total time =  4559.42 ms /  6990 tokens
func parseLlama(source, line, raw string) (db.Record, bool) {
	// Timing lines: indented continuation ("   eval time", "   total time")
	// or the unindented opener ("prompt eval time").
	if llamaTimingRe.MatchString(line) {
		return parseLlamaTiming(source, line, raw)
	}
	return parseLlamaMain(source, line, raw)
}

// mainRe matches: <component>  <func>: <rest>
// component = srv | slot | llm | ggml
// func = alphanumeric+underscores (truncated by llama to ~12 chars)
var llamaMainRe = regexp.MustCompile(`^(srv|slot|llm|ggml)\s{1,12}(\w+):\s*(.*)$`)

// taskRe matches the "id N | task M | message" prefix inside slot lines.
var llamaTaskRe = regexp.MustCompile(`^id\s+(-?\d+)\s+\|\s+task\s+(-?\d+)\s+\|\s*(.*)$`)

// httpRe matches: done request: METHOD PATH CLIENT STATUS
var llamaHTTPRe = regexp.MustCompile(`done request:\s+(\w+)\s+(\S+)\s+(\S+)\s+(\d+)`)

// cacheStateRe matches: - cache state: N prompts, X MiB (limits: ...)
var llamaCacheRe = regexp.MustCompile(`-\s+cache state:\s+(\d+)\s+prompts,\s+([\d.]+)\s+MiB`)

// timingRe matches indented timing lines:
//
//	prompt eval time = 1079.64 ms / 6520 tokens ( 0.17 ms per token, 6039.03 tokens per second)
//	       eval time = 3479.78 ms /  470 tokens ( 7.40 ms per token,  135.07 tokens per second)
//	      total time = 4559.42 ms / 6990 tokens
var llamaTimingRe = regexp.MustCompile(
	`((?:prompt\s+)?eval|total)\s+time\s*=\s*([\d.]+)\s*ms\s*/\s*(\d+)\s+tokens` +
		`(?:\s*\(\s*([\d.]+)\s*ms per token,\s*([\d.]+)\s*tokens per second\))?`,
)

func parseLlamaMain(source, line, raw string) (db.Record, bool) {
	m := llamaMainRe.FindStringSubmatch(line)
	if m == nil {
		return db.Record{}, false
	}
	component, fn, rest := m[1], m[2], strings.TrimSpace(m[3])

	data := map[string]any{
		"component": component,
		"func":      fn,
		"_raw":      raw,
	}

	message := rest
	level := "INFO"

	// Extract task/slot ids from slot lines: "id N | task M | <message>"
	if component == "slot" {
		if tm := llamaTaskRe.FindStringSubmatch(rest); tm != nil {
			if id, err := strconv.ParseInt(tm[1], 10, 64); err == nil {
				data["slot_id"] = id
			}
			if taskID, err := strconv.ParseInt(tm[2], 10, 64); err == nil && taskID >= 0 {
				data["task_id"] = taskID
			}
			message = strings.TrimSpace(tm[3])
		}
	}

	// HTTP completion line
	if hm := llamaHTTPRe.FindStringSubmatch(rest); hm != nil {
		method, path, client, statusStr := hm[1], hm[2], hm[3], hm[4]
		status, _ := strconv.Atoi(statusStr)
		data["method"] = method
		data["path"] = path
		data["client"] = client
		data["status"] = status
		message = method + " " + path + " " + statusStr
		switch {
		case status >= 500:
			level = "ERROR"
		case status >= 400:
			level = "WARN"
		}
	}

	// Cache state summary
	if cm := llamaCacheRe.FindStringSubmatch(rest); cm != nil {
		if n, err := strconv.ParseInt(cm[1], 10, 64); err == nil {
			data["cache_prompts"] = n
		}
		if mib, err := strconv.ParseFloat(cm[2], 64); err == nil {
			data["cache_mib"] = mib
		}
	}

	// Infer level from message content when not already set by HTTP status
	if level == "INFO" {
		lower := strings.ToLower(message)
		if strings.Contains(lower, "error") || strings.Contains(lower, "failed") || strings.Contains(lower, "fatal") {
			level = "ERROR"
		} else if strings.Contains(lower, "warn") {
			level = "WARN"
		}
	}

	// Extract progress percentage
	if strings.Contains(message, "progress =") {
		if pm := regexp.MustCompile(`progress\s*=\s*([\d.]+)`).FindStringSubmatch(message); pm != nil {
			if pct, err := strconv.ParseFloat(pm[1], 64); err == nil {
				data["progress"] = pct
			}
		}
	}

	if message == "" {
		message = fn
	}

	return db.Record{
		Source:  source,
		Level:   level,
		Message: message,
		Data:    data,
	}, true
}

func parseLlamaTiming(source, line, raw string) (db.Record, bool) {
	m := llamaTimingRe.FindStringSubmatch(line)
	if m == nil {
		return db.Record{}, false
	}

	kind := strings.TrimSpace(m[1]) // "prompt eval", "eval", or "total"
	timeMS, _ := strconv.ParseFloat(m[2], 64)
	tokens, _ := strconv.ParseInt(m[3], 10, 64)

	data := map[string]any{
		"kind":      kind,
		"time_ms":   timeMS,
		"tokens":    tokens,
		"_raw":      raw,
		"component": "slot",
		"func":      "print_timing",
	}

	var msg string
	if m[4] != "" && m[5] != "" {
		msPerToken, _ := strconv.ParseFloat(m[4], 64)
		tps, _ := strconv.ParseFloat(m[5], 64)
		data["ms_per_token"] = msPerToken
		data["tokens_per_second"] = tps
		msg = kind + " time: " + m[2] + "ms / " + m[3] + " tokens @ " + m[5] + " t/s"
	} else {
		msg = kind + " time: " + m[2] + "ms / " + m[3] + " tokens"
	}

	return db.Record{
		Source:  source,
		Level:   "INFO",
		Message: msg,
		Data:    data,
	}, true
}
