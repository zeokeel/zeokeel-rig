package memoryvfs

import (
	"context"
	"testing"
)

func TestFuncAdapters(t *testing.T) {
	ctx := context.Background()
	summary, overview, err := SummarizerFunc(func(context.Context, Entry) (string, string, error) {
		return "s", "o", nil
	}).Summarize(ctx, Entry{})
	if err != nil || summary != "s" || overview != "o" {
		t.Fatalf("summarizer = %q %q %v", summary, overview, err)
	}
	candidates, err := ExtractorFunc(func(context.Context, Session) ([]Candidate, error) {
		return []Candidate{{Summary: "c"}}, nil
	}).Extract(ctx, Session{})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("extractor = %+v %v", candidates, err)
	}
}
