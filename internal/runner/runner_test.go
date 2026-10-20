package runner

import (
	"strings"
	"testing"
)

func TestReadEvents(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"thread.started","thread_id":"0199a-abc"}`,
		`{"type":"turn.started"}`,
		`not json at all`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"done"},"usage":{"input_tokens":999}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1200,"cached_input_tokens":800,"output_tokens":90}}`,
		`{"type":"turn.completed","usage":{"input_tokens":300,"cached_input_tokens":200,"output_tokens":10}}`,
	}, "\n")
	res := ReadEvents(strings.NewReader(stream))
	if res.Session != "0199a-abc" {
		t.Errorf("session %q", res.Session)
	}
	want := Usage{Input: 1500, Cached: 1000, Output: 100}
	if res.Usage != want {
		t.Errorf("usage %+v, want %+v (only turn.completed counts)", res.Usage, want)
	}
}

func TestReadEventsEmpty(t *testing.T) {
	if res := ReadEvents(strings.NewReader("")); res.Session != "" || res.Usage != (Usage{}) {
		t.Errorf("%+v", res)
	}
}
