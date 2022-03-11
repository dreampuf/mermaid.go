# mermaid.go

[mermaid.go][] is a library for invoking [mermaid.js][] and getting rending result.

```mermaid
sequenceDiagram
    Actor A as User
    participant B as mermaid.go
    participant C as chromedp

    A ->>+ B: NewRenderEngine()
    B ->>+ C: Lanch new instance of chrome and eval JS library
    C -->> B: 
    B -->> A: 
    
    loop Render Process
        A ->> B: Render()
        B ->> C: mermaid.render()
        C ->> B: { svg, boxModel, exceptions }
        B ->> A: Result{ Svg, BoxModel Error }
    end

    A ->> B: Cancel()
    B -->> C: Context done
    C -->>- C: Shutdown chrome instance
    B -->>- A: 
```

Installation:

```shell
go get -u github.com/dreampuf/mermaid.go
```

Example: 

```go
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
png_in_bytes, box, _ := re.RenderAsPng(content)
```

# How to build

1. Checkout the code base
   `git clone https://github.com/dreampuf/mermaid.go.git`
2. Fetch the latest version of mermaid.js  
    `curl -LO https://unpkg.com/mermaid/dist/mermaid.min.js`
3. Test it  
   `go test ./...`

# License

- [mermaid.go][]: MIT License
- [mermaid.js][]: MIT License
- [chromedp]: MIT License
 
[mermaid.go]: https://github.com/dreampuf/mermaid.go
[mermaid.js]: https://mermaid-js.github.io/mermaid/
[chromedp]: https://github.com/chromedp/chromedp

