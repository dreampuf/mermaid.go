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
	svg_content, _ := re.Render(content)
	// get the result as PNG bytes
	png_in_bytes, _, _ := re.RenderAsPng(content)

	os.WriteFile("example.svg", []byte(svg_content), 0644)

	os.WriteFile("example.png", png_in_bytes, 0644)

}
