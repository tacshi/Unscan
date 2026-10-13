// Unscan is the command line version of the app: it makes scanned PDFs
// searchable, and writes their text, or a text PDF of them.
//
//	go run ./cmd/unscan -o searchable.pdf -txt text.txt -text text.pdf scan.pdf
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"unscan/internal/ocr"
	"unscan/internal/pdfocr"
)

func main() {
	log.SetFlags(0)
	out := flag.String("o", "", "the searchable PDF to write (default: <input> (searchable).pdf)")
	txt := flag.String("txt", "", "a text file to write the text to")
	textPDF := flag.String("text", "", "a text PDF to write: the scanned pages redrawn, their text set in a real font")
	resources := flag.String("resources", "resources", "the directory of the models and ONNX Runtime")
	dpi := flag.Int("dpi", 300, "the resolution pages are read at")
	force := flag.Bool("force", false, "read pages that already have text too")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "usage: unscan [flags] scan.pdf\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	in := flag.Arg(0)
	if *out == "" {
		*out = strings.TrimSuffix(in, filepath.Ext(in)) + " (searchable).pdf"
	}

	paths := ocr.PathsIn(*resources)
	if err := paths.Check(); err != nil {
		log.Fatalf("%v\nrun go run ./cmd/fetchdeps first, or point -resources at the files", err)
	}
	engine, err := ocr.New(paths, ocr.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer engine.Close()
	conv, err := pdfocr.New(engine, pdfocr.DefaultCacheDir())
	if err != nil {
		log.Fatal(err)
	}
	defer conv.Close()
	if *textPDF != "" {
		fonts, err := pdfocr.LoadTextFonts(*resources)
		if err != nil {
			log.Fatalf("%v\nrun go run ./cmd/fetchdeps first, or point -resources at the files", err)
		}
		if err := conv.SetTextFonts(fonts); err != nil {
			log.Fatal(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	opts := pdfocr.Options{DPI: *dpi}
	if *force {
		opts.SkipTextChars = -1
	}
	start := time.Now()
	pages, err := convert(ctx, conv, in, *out, *textPDF, opts)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	lines, skipped := 0, 0
	for _, p := range pages {
		lines += p.Lines
		if p.Skipped {
			skipped++
		}
	}
	log.Printf("%d pages (%d had text already), %d lines in %.1fs → %s",
		len(pages), skipped, lines, time.Since(start).Seconds(), *out)
	if *txt != "" {
		if err := os.WriteFile(*txt, []byte(pdfocr.Text(pages)), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	if *textPDF != "" {
		log.Printf("text PDF → %s", *textPDF)
	}
}

// convert converts the PDF at in, writing the searchable PDF to out and,
// if textPDF is not "", the text PDF there.
func convert(ctx context.Context, conv *pdfocr.Converter, in, out, textPDF string, opts pdfocr.Options) ([]pdfocr.Page, error) {
	f, err := os.Open(in)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var files []*partial
	defer func() {
		for _, p := range files {
			p.discard() // unless finished
		}
	}()
	pdf, err := create(out)
	if err != nil {
		return nil, err
	}
	files = append(files, pdf)
	output := pdfocr.Output{PDF: pdf}
	if textPDF != "" {
		text, err := create(textPDF)
		if err != nil {
			return nil, err
		}
		files = append(files, text)
		output.TextPDF = text
	}
	pages, err := conv.Convert(ctx, f, output, opts, func(done, pages int) {
		fmt.Fprintf(os.Stderr, "\rread %d of %d pages", done, pages)
	})
	if err != nil {
		return nil, err
	}
	for _, p := range files {
		if err := p.finish(); err != nil {
			return nil, err
		}
	}
	return pages, nil
}

// partial is a file written beside where it goes and moved there once
// whole, so that a run that fails or is interrupted leaves no PDF half
// written, nor one there before overwritten; or written where it goes
// directly, where that is no regular file, such as /dev/null or a pipe.
type partial struct {
	*os.File
	path     string
	direct   bool
	finished bool
}

func create(path string) (*partial, error) {
	if fi, err := os.Stat(path); err == nil && !fi.Mode().IsRegular() {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return nil, err
		}
		return &partial{File: f, path: path, direct: true}, nil
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return nil, err
	}
	if err := f.Chmod(0o644); err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	return &partial{File: f, path: path}, nil
}

// finish moves the file where it goes.
func (p *partial) finish() error {
	if err := p.Close(); err != nil {
		return err
	}
	if !p.direct {
		if err := os.Rename(p.Name(), p.path); err != nil {
			return err
		}
	}
	p.finished = true
	return nil
}

// discard removes the file, unless it was finished.
func (p *partial) discard() {
	if !p.finished {
		p.Close()
		if !p.direct {
			os.Remove(p.Name())
		}
	}
}
