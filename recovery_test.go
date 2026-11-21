package rig_test

import (
	"errors"
	"testing"

	"github.com/zeokeel/zeokeel-rig"
)

func TestIsContextOverflowError(t *testing.T) {
	positives := []string{
		"openai: context_length_exceeded",
		"maximum context length is 128000 tokens",
		"this model's maximum context length",
		"the request exceeds the context window",
		"too many tokens in the prompt",
		"prompt is too long",
		"please reduce the length of the messages",
	}
	for _, text := range positives {
		if !rig.IsContextOverflowError(errors.New(text)) {
			t.Errorf("%q not recognized as a context overflow", text)
		}
	}
	negatives := []string{
		"429 rate limit exceeded",
		"401 unauthorized",
		"connection reset by peer",
		"invalid tool arguments",
	}
	for _, text := range negatives {
		if rig.IsContextOverflowError(errors.New(text)) {
			t.Errorf("%q wrongly recognized as a context overflow", text)
		}
	}
	if rig.IsContextOverflowError(nil) {
		t.Error("nil error reported as a context overflow")
	}
}
