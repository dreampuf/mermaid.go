package main

import (
	"context"
	"os"

	"github.com/dreampuf/mermaid.go"
)

func main() {

	ctx := context.Background()
	re, _ := mermaid_go.NewRenderEngine(ctx)
	defer re.Cancel()

	content := `graph TD;
    A-->B;
    A-->C;
    B-->D;
    C-->D;`

	// get the render result in SVG/XML string
	svg_content, _, _, _ := re.Render(content)
	// get the result as PNG bytes
	png_in_bytes, _, _ := re.RenderAsPng(content)

	scaled_png_in_bytes, _, _ := re.RenderAsScaledPng(content, 2.0)

	err := os.WriteFile("example.svg", []byte(svg_content), 0644)
	if err != nil {
		os.Exit(1)
	}

	err = os.WriteFile("example.png", png_in_bytes, 0644)
	if err != nil {
		os.Exit(1)
	}

	err = os.WriteFile("example_scaled.png", scaled_png_in_bytes, 0644)
	if err != nil {
		os.Exit(1)
	}
}
