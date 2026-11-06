package web_test

// Unit tests for the stdlib-only HTML-to-text extractor.

import (
	"strings"
	"testing"

	"github.com/zeokeel/zeokeel-rig/tool/web"
)

func TestHTMLToText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain text",
			in:   "just text",
			want: "just text",
		},
		{
			name: "tags removed and whitespace merged",
			in:   "<p>Hello   <b>world</b>!</p>",
			want: "Hello world!",
		},
		{
			name: "block elements break lines",
			in:   "<h1>Title</h1><p>one</p><p>two</p>",
			want: "Title\none\ntwo",
		},
		{
			name: "br breaks a line",
			in:   "a<br>b<br/>c",
			want: "a\nb\nc",
		},
		{
			name: "script and style dropped",
			in:   "<p>keep</p><script>var x = 1;</script><style>.x{}</style><p>this</p>",
			want: "keep\nthis",
		},
		{
			name: "entities decoded",
			in:   "<p>A &amp; B &lt;tag&gt; &#39;q&#39; &quot;d&quot;</p>",
			want: "A & B <tag> 'q' \"d\"",
		},
		{
			name: "non-breaking space",
			in:   "<p>a&nbsp;b</p>",
			want: "a b",
		},
		{
			name: "link text preserved",
			in:   `<p>see <a href="https://example.com">the docs</a> now</p>`,
			want: "see the docs now",
		},
		{
			name: "comments removed",
			in:   "<p>before<!-- hidden -->after</p>",
			want: "beforeafter",
		},
		{
			name: "malformed markup kept as text",
			in:   "<p>ok</p><broken",
			want: "ok\n<broken",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
		{
			name: "nested script with attributes",
			in:   `<p>visible</p><script type="text/javascript">if (a < b) {}</script>`,
			want: "visible",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := web.HTMLToText(testCase.in)
			if got != testCase.want {
				t.Fatalf("HTMLToText(%q) = %q, want %q", testCase.in, got, testCase.want)
			}
		})
	}
}

func TestHTMLToTextHandlesLargeAndNoisyInput(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("<html><body>")
	for index := 0; index < 500; index++ {
		builder.WriteString("<div class=x>item ")
		builder.WriteString(strings.Repeat("y", 20))
		builder.WriteString("</div>")
	}
	builder.WriteString("</body></html>")
	text := web.HTMLToText(builder.String())
	if strings.Count(text, "item") != 500 {
		t.Fatalf("extracted item count = %d, want 500", strings.Count(text, "item"))
	}
	if strings.Contains(text, "<div") || strings.Contains(text, "</") {
		t.Fatalf("extracted text still contains markup: %q", text[:200])
	}
}
