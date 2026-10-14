// Unscan is the command line version of the app: it makes scanned PDFs
// searchable, and writes their text, or a text PDF of them, in the text
// fonts or fonts of the user's.
//
//	go run ./cmd/unscan -o searchable.pdf -txt text.txt -text text.pdf scan.pdf
//	go run ./cmd/unscan -text text.pdf -serif Songti.ttc -serif-bold Songti.ttc scan.pdf
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
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
	cpu := flag.Bool("cpu", false, "run every model on the CPU, not the text models on the GPU through Core ML")
	var fontFiles [len(userFonts)]string
	for i, f := range userFonts {
		flag.StringVar(&fontFiles[i], f.flag, "", fmt.Sprintf("a font file to set the text PDF's %s lines in, "+
			"instead of %s: TrueType (.ttf), or a collection (.ttc), file.ttc#n for its font n from 0", f.lines, f.builtIn))
	}
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: unscan [flags] scan.pdf\n")
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
	engine, err := ocr.New(paths, ocr.Options{CPUOnly: *cpu, CacheDir: ocr.DefaultCacheDir()})
	if err != nil {
		log.Fatal(err)
	}
	defer engine.Close()
	conv, err := pdfocr.New(engine, pdfocr.DefaultCacheDir())
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = conv.Close() }()
	if *textPDF != "" {
		fonts, err := pdfocr.LoadTextFonts(*resources)
		if err != nil {
			log.Fatalf("%v\nrun go run ./cmd/fetchdeps first, or point -resources at the files", err)
		}
		if err := conv.SetTextFonts(fonts); err != nil {
			log.Fatal(err)
		}
		user, err := readUserFonts(fontFiles)
		if err != nil {
			log.Fatal(err)
		}
		if err := conv.SetUserFonts(user); err != nil {
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
	lines, skipped, layered := 0, 0, 0
	for _, p := range pages {
		lines += p.Lines
		if p.Skipped {
			skipped++
		}
		if p.Layered {
			layered++
		}
	}
	log.Printf("%d pages (%d had text already, %d a scan's text layer, read again for the text PDF), %d lines in %.1fs → %s",
		len(pages), skipped, layered, lines, time.Since(start).Seconds(), *out)
	if *txt != "" {
		if err := os.WriteFile(*txt, []byte(pdfocr.Text(pages)), 0o644); err != nil { // #nosec G306 -- the user's document
			log.Fatal(err)
		}
	}
	if *textPDF != "" {
		log.Printf("text PDF → %s", *textPDF)
	}
}

// userFonts are the faces of the text PDF that the user may give fonts of
// their own for, by flag, in the order of pdfocr.TextFonts.
var userFonts = [...]struct {
	flag, lines, builtIn string
	bold                 bool
}{
	{"serif", "serif", "Noto Serif SC", false},
	{"serif-bold", "bold serif", "Noto Serif SC Bold", true},
	{"sans", "sans-serif", "Noto Sans SC", false},
	{"sans-bold", "bold sans-serif", "Noto Sans SC Bold", true},
	{"kai", "Kai (楷体)", "LXGW WenKai", false},
}

// readUserFonts reads the font files given by flag, each a path, or a
// collection's path and #n for its font n: without, the one the face's
// weight picks.
func readUserFonts(files [len(userFonts)]string) (pdfocr.TextFonts, error) {
	var fonts [len(userFonts)]pdfocr.Font
	for i, arg := range files {
		if arg == "" {
			continue
		}
		path, index := arg, -1
		if at := strings.LastIndexByte(arg, '#'); at >= 0 {
			if _, err := os.Stat(arg); err != nil {
				if n, err := strconv.Atoi(arg[at+1:]); err == nil && n >= 0 {
					path, index = arg[:at], n
				}
			}
		}
		f, names, err := pdfocr.ReadFont(path, index, userFonts[i].bold)
		if err != nil {
			return pdfocr.TextFonts{}, fmt.Errorf("-%s %s: %w", userFonts[i].flag, arg, err)
		}
		if len(names) > 1 {
			log.Printf("%s lines in %s, font %d of %s's %d", userFonts[i].lines, names[f.Index], f.Index,
				filepath.Base(path), len(names))
		}
		fonts[i] = f
	}
	return pdfocr.TextFonts{Serif: fonts[0], SerifBold: fonts[1], Sans: fonts[2], SansBold: fonts[3], Kai: fonts[4]}, nil
}

// convert converts the PDF at in, writing the searchable PDF to out and,
// if textPDF is not "", the text PDF there.
func convert(ctx context.Context, conv *pdfocr.Converter, in, out, textPDF string, opts pdfocr.Options) ([]pdfocr.Page, error) {
	f, err := os.Open(in) // #nosec G304 -- the PDF given
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read only
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
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0) // #nosec G304 -- where the user writes it
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
		_ = f.Close()
		_ = os.Remove(f.Name())
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
		_ = p.Close()
		if !p.direct {
			_ = os.Remove(p.Name())
		}
	}
}
