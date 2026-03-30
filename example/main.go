package main

import (
	"context"
	"os"

	"github.com/dreampuf/mermaid.go"
)

func main() {

	ctx := context.Background()
	re, err := mermaid_go.NewRenderEngine(ctx, nil)
	if err != nil {
		panic(err)
	}
	defer re.Cancel()

	content := `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`

	// get the render result in SVG/XML string
	svg_content, err := re.Render(content, mermaid_go.WithBundle())
	if err != nil {
		panic(err)
	}
	// get the result as PNG bytes
	png_in_bytes, _, err := re.RenderAsPng(content)
	if err != nil {
		panic(err)
	}

	scaled_png_in_bytes, _, err := re.RenderAsScaledPng(content, 2.0)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile("example.svg", []byte(svg_content), 0644)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile("example.png", png_in_bytes, 0644)
	if err != nil {
		panic(err)
	}

	err = os.WriteFile("example_scaled.png", scaled_png_in_bytes, 0644)
	if err != nil {
		panic(err)
	}
}
