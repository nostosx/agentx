package htmltext

import "testing"

func TestText(t *testing.T) {
	in := `<html><head><title>T</title><style>p{}</style><script>var x=1</script></head>
<body><!-- note --><h1>Title</h1><p>One &amp; <b>two</b></p>
<ul><li>a</li><li>b</li></ul>text<br>next</body></html>`
	want := "T\nTitle\nOne & two\na\nb\ntext\nnext"
	if got := Text(in); got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
}

func TestTitle(t *testing.T) {
	cases := map[string]string{
		"<html><head><title>\n  A &amp; B\n</title></head></html>": "A & B",
		"<TITLE lang=en>Up</TITLE>":                                "Up",
		"<p>no title</p>":                                          "",
	}
	for in, want := range cases {
		if got := Title(in); got != want {
			t.Errorf("Title(%q) = %q, want %q", in, got, want)
		}
	}
}
