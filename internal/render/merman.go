package render

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"image/png"
	"math"
	"strings"

	"github.com/chromedp/cdproto/dom"
)

// MermanSVG finishes an SVG render under either merman backend, bundling the
// source when the call asked for it.
func MermanSVG(out []byte, content string, opts *Options) (string, error) {
	svg := string(out)
	if opts.Bundle {
		return BundleSource(svg, content)
	}
	return svg, nil
}

// MermanPngOptions applies a PNG render's options and checks its scale, as both
// merman backends do before rendering anything.
func MermanPngOptions[O ~func(*Options)](opts []O, scale float64) (*Options, error) {
	renderOpts := NewOptions(opts)
	if renderOpts.Bundle {
		return nil, fmt.Errorf("%w: WithBundle has no effect on a PNG, the source is embedded in the SVG's <desc>", ErrUnsupportedOption)
	}
	// NaN and +Inf both compare false to <= 0, so they need naming: they would
	// otherwise reach merman as "NaN"/"+Inf" and come back as a rejected request
	// rather than the unsupported scale this documents.
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale <= 0 {
		return nil, fmt.Errorf("%w: scale must be a positive, finite number, got %v", ErrUnsupportedOption, scale)
	}
	return renderOpts, nil
}

// MermanPng pairs a PNG merman rendered with its box model, derived from the
// image at scale since merman reports no layout box. from names the renderer
// in the error for an image that does not decode.
func MermanPng(out []byte, scale float64, from string) ([]byte, *dom.BoxModel, error) {
	config, err := png.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s returned an unreadable png: %w", ErrMermanFailed, from, err)
	}
	return out, BoxModelFor(float64(config.Width)/scale, float64(config.Height)/scale), nil
}

// BoxModelFor describes a w x h box at the origin, the shape chrome's
// DOM.getBoxModel returns for a diagram with no offset or spacing around it.
func BoxModelFor(w, h float64) *dom.BoxModel {
	quad := dom.Quad{0, 0, w, 0, w, h, 0, h}
	return &dom.BoxModel{
		Content: quad,
		Padding: quad,
		Border:  quad,
		Margin:  quad,
		Width:   int64(math.Round(w)),
		Height:  int64(math.Round(h)),
	}
}

// BundleSource embeds the diagram source in the SVG's <desc>, as WithBundle does
// under the chrome backend by way of the DOM. There is no DOM here, so the
// element is spliced in after the root start tag, which is enough: <desc> is
// valid as the first child of <svg> and the source is XML-escaped on the way in.
func BundleSource(svg, content string) (string, error) {
	open := strings.Index(svg, "<svg")
	if open < 0 {
		return "", fmt.Errorf("%w: rendered output has no <svg> element to bundle the source into", ErrMermanFailed)
	}
	end, selfClosing := endOfStartTag(svg[open:])
	if end < 0 {
		return "", fmt.Errorf("%w: rendered output has an unterminated <svg> start tag", ErrMermanFailed)
	}
	end += open

	var desc bytes.Buffer
	desc.WriteString("<desc>")
	if err := xml.EscapeText(&desc, []byte(content)); err != nil {
		return "", fmt.Errorf("%w: %w", ErrFailedEncoding, err)
	}
	desc.WriteString("</desc>")

	var out strings.Builder
	out.Grow(len(svg) + desc.Len() + len("</svg>"))
	if selfClosing {
		// <svg .../> has nowhere to put a child, so give it a body to hold one.
		out.WriteString(svg[:end-1])
		out.WriteString(">")
		out.WriteString(desc.String())
		out.WriteString("</svg>")
		out.WriteString(svg[end+1:])
		return out.String(), nil
	}
	out.WriteString(svg[:end+1])
	out.WriteString(desc.String())
	out.WriteString(svg[end+1:])
	return out.String(), nil
}

// endOfStartTag returns the index of the '>' closing the start tag that s begins
// with, ignoring any '>' inside a quoted attribute value, and whether the tag is
// self-closing. It returns -1 if the tag is never closed.
func endOfStartTag(s string) (index int, selfClosing bool) {
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			return i, i > 0 && s[i-1] == '/'
		}
	}
	return -1, false
}
