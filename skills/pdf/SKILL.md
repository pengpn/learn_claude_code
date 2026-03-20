---
name: pdf
description: Process PDF files - extract text, create PDFs, merge documents. Use when user asks to read PDF, create PDF, or work with PDF files.
---

# PDF Processing Skill

You now have expertise in PDF manipulation using Go. Follow these workflows:

## Reading PDFs

**Option 1: Quick text extraction with CLI (preferred)**
```bash
# Using pdftotext (poppler-utils)
pdftotext input.pdf -  # Output to stdout
pdftotext input.pdf output.txt  # Output to file
```

**Option 2: Programmatic extraction with Go**
```go
package main

import (
	"fmt"
	"os"

	"github.com/ledongthuc/pdf"
)

func extractText(path string) (string, error) {
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var text string
	for i := 1; i <= r.NumPage(); i++ {
		p := r.Page(i)
		content, err := p.GetPlainText(nil)
		if err != nil {
			continue
		}
		text += fmt.Sprintf("--- Page %d ---\n%s\n", i, content)
	}
	return text, nil
}
```

**Option 3: Using pdfcpu for metadata**
```go
import "github.com/pdfcpu/pdfcpu/pkg/api"

// Get PDF info
api.InfoFile("input.pdf", nil, nil)

// Extract text
api.ExtractContentFile("input.pdf", "output_dir", nil, nil)
```

## Creating PDFs

**Option 1: From Markdown (recommended)**
```bash
# Using pandoc
pandoc input.md -o output.pdf

# With custom styling
pandoc input.md -o output.pdf --pdf-engine=xelatex -V geometry:margin=1in
```

**Option 2: Programmatically with Go**
```go
package main

import "github.com/signintech/gopdf"

func createPDF(outputPath string) error {
	pdf := gopdf.GoPdf{}
	pdf.Start(gopdf.Config{PageSize: *gopdf.PageSizeA4})
	pdf.AddPage()

	err := pdf.AddTTFFont("default", "/path/to/font.ttf")
	if err != nil {
		return err
	}
	pdf.SetFont("default", "", 14)

	pdf.SetX(50)
	pdf.SetY(50)
	pdf.Cell(nil, "Hello, PDF!")

	return pdf.WritePdf(outputPath)
}
```

**Option 3: Using gofpdf**
```go
import "github.com/jung-kurt/gofpdf"

func createSimplePDF(outputPath string) error {
	pdf := gofpdf.New("P", "mm", "A4", "")
	pdf.AddPage()
	pdf.SetFont("Arial", "B", 16)
	pdf.Cell(40, 10, "Hello, PDF!")
	return pdf.OutputFileAndClose(outputPath)
}
```

## Merging PDFs

```go
import "github.com/pdfcpu/pdfcpu/pkg/api"

func mergePDFs(inputs []string, output string) error {
	return api.MergeCreateFile(inputs, output, false, nil)
}

// Usage:
// mergePDFs([]string{"file1.pdf", "file2.pdf", "file3.pdf"}, "merged.pdf")
```

## Splitting PDFs

```go
import "github.com/pdfcpu/pdfcpu/pkg/api"

func splitPDF(input, outputDir string) error {
	// Split into single pages
	return api.SplitFile(input, outputDir, 1, nil)
}
```

## Key Libraries

| Task | Library | Install |
|------|---------|---------|
| Read/Parse | ledongthuc/pdf | `go get github.com/ledongthuc/pdf` |
| Merge/Split/Manipulate | pdfcpu | `go get github.com/pdfcpu/pdfcpu` |
| Create (basic) | gofpdf | `go get github.com/jung-kurt/gofpdf` |
| Create (CJK/advanced) | gopdf | `go get github.com/signintech/gopdf` |
| Text extraction (CLI) | pdftotext | `brew install poppler` / `apt install poppler-utils` |

## Best Practices

1. **Always check if tools are installed** before using them
2. **Handle encoding issues** - PDFs may contain various character encodings
3. **Large PDFs**: Process page by page to avoid memory issues
4. **OCR for scanned PDFs**: Use external tools like `tesseract` if text extraction returns empty
5. **pdfcpu CLI**: Also available as CLI tool for quick operations (`go install github.com/pdfcpu/pdfcpu/cmd/pdfcpu@latest`)
