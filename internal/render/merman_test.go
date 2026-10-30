package render

import (
	"errors"
	"testing"
)

func TestBundleSource(t *testing.T) {
	cases := []struct {
		name, svg, want string
		wantErr         bool
	}{
		{
			name: "PlainRoot",
			svg:  `<svg id="mermaid"><g/></svg>`,
			want: `<svg id="mermaid"><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "AttributeContainingAngleBrackets",
			// A '>' inside an attribute value must not be mistaken for the end
			// of the start tag, or the <desc> lands inside the attribute.
			svg:  `<svg data-note="a > b"><g/></svg>`,
			want: `<svg data-note="a > b"><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "XMLPrologue",
			svg:  `<?xml version="1.0"?><svg><g/></svg>`,
			want: `<?xml version="1.0"?><svg><desc>a-&gt;b</desc><g/></svg>`,
		},
		{
			name: "SelfClosingRoot",
			svg:  `<svg width="10"/>`,
			want: `<svg width="10"><desc>a-&gt;b</desc></svg>`,
		},
		{
			// XML allows space before the '/' but not between '/' and '>'
			// (EmptyElemTag ::= '<' Name (S Attribute)* S? '/>'), so looking at
			// the character before '>' is enough to recognise every valid form.
			name: "SelfClosingRootWithSpace",
			svg:  `<svg width="10" />`,
			want: `<svg width="10" ><desc>a-&gt;b</desc></svg>`,
		},
		{
			name: "SelfClosingRootAcrossLines",
			svg:  "<svg width=\"10\"\n/>",
			want: "<svg width=\"10\"\n><desc>a-&gt;b</desc></svg>",
		},
		{name: "NotAnSvg", svg: "kaboom", wantErr: true},
		{name: "UnterminatedStartTag", svg: `<svg id="mermaid"`, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BundleSource(tc.svg, "a->b")
			if tc.wantErr {
				if !errors.Is(err, ErrMermanFailed) {
					t.Fatalf("BundleSource() error = %v, want ErrMermanFailed", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("BundleSource() error = %v", err)
			}
			if got != tc.want {
				t.Errorf("BundleSource() = %q, want %q", got, tc.want)
			}
		})
	}
}
