package parser

import (
	"testing"
)

// All test lines are real output captured from the llama.cpp server.

func TestLlama_httpCompletion_200(t *testing.T) {
	raw := `srv  log_server_r: done request: POST /v1/chat/completions 172.18.0.11 200`
	r := Parse("llama", raw)

	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
	if r.Data["method"] != "POST" {
		t.Errorf("method = %v, want POST", r.Data["method"])
	}
	if r.Data["path"] != "/v1/chat/completions" {
		t.Errorf("path = %v, want /v1/chat/completions", r.Data["path"])
	}
	if r.Data["status"] != 200 {
		t.Errorf("status = %v, want 200", r.Data["status"])
	}
	if r.Data["client"] != "172.18.0.11" {
		t.Errorf("client = %v, want 172.18.0.11", r.Data["client"])
	}
	if r.Data["component"] != "srv" {
		t.Errorf("component = %v, want srv", r.Data["component"])
	}
}

func TestLlama_httpCompletion_4xx(t *testing.T) {
	raw := `srv  log_server_r: done request: POST /v1/chat/completions 172.18.0.11 422`
	r := Parse("llama", raw)
	if r.Level != "WARN" {
		t.Errorf("level = %q, want WARN for 422", r.Level)
	}
	if r.Data["status"] != 422 {
		t.Errorf("status = %v, want 422", r.Data["status"])
	}
}

func TestLlama_httpCompletion_5xx(t *testing.T) {
	raw := `srv  log_server_r: done request: POST /v1/completions 172.18.0.11 500`
	r := Parse("llama", raw)
	if r.Level != "ERROR" {
		t.Errorf("level = %q, want ERROR for 500", r.Level)
	}
}

func TestLlama_slotTask(t *testing.T) {
	raw := `slot update_slots: id  0 | task 7253 | new prompt, n_ctx_slot = 32768, n_keep = 0, task.n_tokens = 7571`
	r := Parse("llama", raw)

	if r.Data["slot_id"] != int64(0) {
		t.Errorf("slot_id = %v, want 0", r.Data["slot_id"])
	}
	if r.Data["task_id"] != int64(7253) {
		t.Errorf("task_id = %v, want 7253", r.Data["task_id"])
	}
	if r.Data["component"] != "slot" {
		t.Errorf("component = %v, want slot", r.Data["component"])
	}
	if r.Data["func"] != "update_slots" {
		t.Errorf("func = %v, want update_slots", r.Data["func"])
	}
}

func TestLlama_slotTask_noTask(t *testing.T) {
	// task -1 means no active task (idle slot)
	raw := `slot launch_slot_: id  0 | task -1 | sampler chain: logits -> top-k -> top-p`
	r := Parse("llama", raw)

	if r.Data["slot_id"] != int64(0) {
		t.Errorf("slot_id = %v, want 0", r.Data["slot_id"])
	}
	// task -1 should not be stored (sentinel for no task)
	if _, ok := r.Data["task_id"]; ok {
		t.Errorf("task_id should not be set for task -1, got %v", r.Data["task_id"])
	}
}

func TestLlama_progressLine(t *testing.T) {
	raw := `slot update_slots: id  0 | task 7253 | prompt processing progress, n_tokens = 3099, batch.n_tokens = 2048, progress = 0.409325`
	r := Parse("llama", raw)

	pct, ok := r.Data["progress"].(float64)
	if !ok {
		t.Fatalf("progress not parsed, data = %v", r.Data)
	}
	if pct < 0.40 || pct > 0.42 {
		t.Errorf("progress = %v, want ~0.409", pct)
	}
}

func TestLlama_srvGeneric(t *testing.T) {
	raw := `srv  update_slots: all slots are idle`
	r := Parse("llama", raw)

	if r.Data["component"] != "srv" {
		t.Errorf("component = %v, want srv", r.Data["component"])
	}
	if r.Message != "all slots are idle" {
		t.Errorf("message = %q, want 'all slots are idle'", r.Message)
	}
	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
}

func TestLlama_cacheState(t *testing.T) {
	raw := `srv        update:  - cache state: 9 prompts, 1114.677 MiB (limits: 8192.000 MiB, 32768 tokens, 142089 est)`
	r := Parse("llama", raw)

	if r.Data["cache_prompts"] != int64(9) {
		t.Errorf("cache_prompts = %v, want 9", r.Data["cache_prompts"])
	}
	mib, ok := r.Data["cache_mib"].(float64)
	if !ok || mib < 1114 || mib > 1115 {
		t.Errorf("cache_mib = %v, want ~1114.677", r.Data["cache_mib"])
	}
}

func TestLlama_timingPromptEval(t *testing.T) {
	raw := `prompt eval time =    1079.64 ms /  6520 tokens (    0.17 ms per token,  6039.03 tokens per second)`
	r := Parse("llama", raw)

	if r.Level != "INFO" {
		t.Errorf("level = %q, want INFO", r.Level)
	}
	if r.Data["kind"] != "prompt eval" {
		t.Errorf("kind = %v, want 'prompt eval'", r.Data["kind"])
	}
	ms, ok := r.Data["time_ms"].(float64)
	if !ok || ms < 1079 || ms > 1080 {
		t.Errorf("time_ms = %v, want ~1079.64", r.Data["time_ms"])
	}
	if r.Data["tokens"] != int64(6520) {
		t.Errorf("tokens = %v, want 6520", r.Data["tokens"])
	}
	tps, ok := r.Data["tokens_per_second"].(float64)
	if !ok || tps < 6039 || tps > 6040 {
		t.Errorf("tokens_per_second = %v, want ~6039.03", r.Data["tokens_per_second"])
	}
}

func TestLlama_timingEval(t *testing.T) {
	raw := `       eval time =    3479.78 ms /   470 tokens (    7.40 ms per token,   135.07 tokens per second)`
	r := Parse("llama", raw)

	if r.Data["kind"] != "eval" {
		t.Errorf("kind = %v, want 'eval'", r.Data["kind"])
	}
	if r.Data["tokens"] != int64(470) {
		t.Errorf("tokens = %v, want 470", r.Data["tokens"])
	}
}

func TestLlama_timingTotal(t *testing.T) {
	raw := `      total time =    4559.42 ms /  6990 tokens`
	r := Parse("llama", raw)

	if r.Data["kind"] != "total" {
		t.Errorf("kind = %v, want 'total'", r.Data["kind"])
	}
	ms, ok := r.Data["time_ms"].(float64)
	if !ok || ms < 4559 || ms > 4560 {
		t.Errorf("time_ms = %v, want ~4559.42", r.Data["time_ms"])
	}
	// total line has no tps
	if _, ok := r.Data["tokens_per_second"]; ok {
		t.Errorf("tokens_per_second should not be set on total line")
	}
}

func TestLlama_slotRelease(t *testing.T) {
	raw := `slot      release: id  0 | task 7253 | stop processing: n_tokens = 8040, truncated = 0`
	r := Parse("llama", raw)

	if r.Data["slot_id"] != int64(0) {
		t.Errorf("slot_id = %v, want 0", r.Data["slot_id"])
	}
	if r.Data["task_id"] != int64(7253) {
		t.Errorf("task_id = %v, want 7253", r.Data["task_id"])
	}
}

func TestLlama_chatFormat(t *testing.T) {
	raw := `srv  params_from_: Chat format: peg-gemma4`
	r := Parse("llama", raw)

	if r.Data["component"] != "srv" {
		t.Errorf("component = %v, want srv", r.Data["component"])
	}
	if r.Message != "Chat format: peg-gemma4" {
		t.Errorf("message = %q, want 'Chat format: peg-gemma4'", r.Message)
	}
}

func TestLlama_notMatchedByOtherParsers(t *testing.T) {
	// Ensure llama lines aren't accidentally caught by the uvicorn or node-router parsers
	llamaLines := []string{
		`srv  log_server_r: done request: POST /v1/chat/completions 172.18.0.11 200`,
		`slot update_slots: id  0 | task 7253 | new prompt, n_ctx_slot = 32768`,
		`       eval time =    3479.78 ms /   470 tokens (    7.40 ms per token,   135.07 tokens per second)`,
	}
	for _, line := range llamaLines {
		_, ok := parseUvicorn("llama", line, line)
		if ok {
			t.Errorf("uvicorn parser should not match llama line: %q", line)
		}
		_, ok = parseNodeRouter("llama", line, line)
		if ok {
			t.Errorf("node-router parser should not match llama line: %q", line)
		}
	}
}
