// Unscan turns scanned PDFs into searchable ones on the user's machine:
// PP-OCRv6 reads each page, and its text goes invisibly over the page. It
// also retypes them as a text PDF, the text set in a real font.
package main

import (
	"context"
	"fmt"
	"image"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"unscan/internal/ocr"
	"unscan/internal/pdfocr"
)

type phase int

const (
	idle    phase = iota // waiting for a PDF
	working              // reading one
	done                 // showing what was read
)

// The forms a document shows in, and is saved in.
const (
	searchable = iota // the scan, with its text invisibly over it
	retyped           // the text PDF
	plainText         // the text alone
)

// previewSize is the longest side of a page's preview, in pixels: sharp
// on a Retina display in all but the largest windows.
const previewSize = 2000

// effects are what the app does outside its state; tests replace them.
type effects struct {
	update  func(func()) // run on the main thread, then redraw
	spawn   func(func()) // run in the background
	convert func(ctx context.Context, path string, progress pdfocr.Progress) (*result, error)
	render  func(path string, page int) (image.Image, error) // a page's preview
	open    func() (string, error)                           // ask for a PDF; "" when canceled
	saveAs  func(name string) (string, error)
	reveal  func(path string)      // show a file in the file manager
	menu    func(open, save bool)  // enable the menu bar's Open and Save
	dock    func(progress float64) // the Dock's progress bar; negative removes it
	attend  func()                 // draw the user back to the window
}

// app is the state the window shows.
type app struct {
	fx effects

	phase  phase
	broken string // why the models could not load, so that nothing can be read
	err    string // why the last PDF could not be read
	toast  *toast // to show in the next build

	file        string    // the PDF being read
	waiting     bool      // file waits for the models to load
	page, pages int       // pages read so far, of pages
	counted     time.Time // when the pages were counted
	cancel      context.CancelFunc
	run         int // counts runs, so that a canceled one's result is dropped

	result    *result   // the document shown
	form      int       // searchable, retyped or plainText
	at        int       // the page shown
	pageArea  ui.Handle // where the page shows, whose arrows turn it
	previews  map[preview]rendered
	rendering bool // a preview is being rendered

	menuOpen, menuSave bool // what the menu bar was last told
}

// toast is a message about what the user just did.
type toast struct {
	title, detail string
	saved         string // the file saved, to show in the file manager
}

// preview names a page's preview: the page in a form.
type preview struct{ form, page int }

type rendered struct {
	bitmap *ui.Bitmap
	err    error
}

// result is what reading a PDF made: its pages' text, and the PDFs, which
// stay in temporary files until the user saves them, as a long document's
// would fill memory.
type result struct {
	file    string // the PDF read
	pages   []pdfocr.Page
	pdf     string // the searchable PDF
	textPDF string // the text PDF
	dir     string // the directory they are in, removed with them, or ""
}

// remove removes the result's files.
func (r *result) remove() {
	if r != nil && r.dir != "" {
		os.RemoveAll(r.dir)
	}
}

// canOpen reports whether a PDF can be opened now.
func (a *app) canOpen() bool { return a.phase != working && a.broken == "" }

// choose asks for a PDF and reads it.
func (a *app) choose() {
	if !a.canOpen() {
		return
	}
	a.fx.spawn(func() {
		path, err := a.fx.open()
		a.fx.update(func() {
			switch {
			case err != nil:
				a.toast = &toast{title: "Couldn’t open the PDF", detail: err.Error()}
			case path != "":
				a.start(path)
			}
		})
	})
}

// drop reads the first PDF of files dropped on the window.
func (a *app) drop(files []string) {
	for _, f := range files {
		if strings.EqualFold(filepath.Ext(f), ".pdf") {
			a.start(f)
			return
		}
	}
	a.toast = &toast{title: fmt.Sprintf("“%s” isn’t a PDF", filepath.Base(files[0]))}
}

// start reads the PDF at path, once the models have loaded. The document
// shown stays until it is read.
func (a *app) start(path string) {
	if !a.canOpen() {
		return
	}
	a.run++
	a.phase, a.err, a.file = working, "", path
	a.page, a.pages, a.counted = 0, 0, time.Time{}
	a.waiting = a.fx.convert == nil
	if !a.waiting {
		a.read()
	}
}

// loaded is called once the models have loaded, with the error if they
// could not; the PDF chosen meanwhile is read then.
func (a *app) loaded(err error) {
	waiting := a.waiting
	a.waiting = false
	switch {
	case err != nil:
		a.broken = err.Error()
		if waiting {
			a.back()
		}
	case waiting:
		a.read()
	}
}

// read reads a.file in the background.
func (a *app) read() {
	run := a.run
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	path, convert := a.file, a.fx.convert
	a.fx.spawn(func() {
		defer cancel()
		res, err := convert(ctx, path, func(page, pages int) {
			a.fx.update(func() {
				if a.run != run {
					return
				}
				if a.counted.IsZero() {
					a.counted = time.Now()
				}
				a.page, a.pages = page, pages
				a.fx.dock(float64(page) / float64(max(pages, 1)))
			})
		})
		a.fx.update(func() {
			if a.run != run {
				res.remove()
				return
			}
			a.cancel = nil
			a.fx.dock(-1)
			a.fx.attend()
			if err != nil {
				a.back()
				msg := fmt.Sprintf("Couldn’t read “%s”", filepath.Base(path))
				if a.phase == done {
					a.toast = &toast{title: msg, detail: err.Error()}
				} else {
					a.err = msg + ": " + err.Error()
				}
				return
			}
			res.file = path
			a.result.remove()
			a.phase, a.result = done, res
			a.form, a.at = searchable, 0
			a.previews, a.rendering = map[preview]rendered{}, false
		})
	})
}

// stop stops reading, and goes back to the document shown before, if any.
func (a *app) stop() {
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.run++
	a.waiting = false
	a.fx.dock(-1)
	a.back()
}

// back shows the document read last, or asks for one.
func (a *app) back() {
	a.phase = idle
	if a.result != nil {
		a.phase = done
	}
}

// turn shows page i, within the document.
func (a *app) turn(i int) {
	a.at = min(max(i, 0), len(a.result.pages)-1)
}

// renderPreviews renders the preview the document view needs next, one at
// a time: the page shown, then those beside it, so that turning to them
// shows them at once. It forgets those further away.
func (a *app) renderPreviews() {
	if a.phase != done || a.form == plainText || a.rendering {
		return
	}
	r := a.result
	path := r.pdf
	if a.form == retyped {
		path = r.textPDF
	}
	for _, page := range []int{a.at, a.at + 1, a.at - 1} {
		key := preview{a.form, page}
		if _, ok := a.previews[key]; ok || page < 0 || page >= len(r.pages) {
			continue
		}
		a.rendering = true
		render := a.fx.render
		a.fx.spawn(func() {
			img, err := render(path, page)
			a.fx.update(func() {
				if a.result != r {
					return
				}
				a.rendering = false
				p := rendered{err: err}
				if err == nil {
					p.bitmap = ui.NewBitmap(img)
				}
				a.previews[key] = p
				for k := range a.previews {
					if k.page < a.at-1 || k.page > a.at+1 {
						delete(a.previews, k)
					}
				}
			})
		})
		return
	}
}

// save asks where to save the document in the form shown, and saves it
// there.
func (a *app) save() {
	if a.phase != done {
		return
	}
	r := a.result
	// Beside the PDF read, unless the user chooses otherwise: a bare name
	// would put the dialog in the app's working directory, which is / for
	// an app opened from the Finder.
	base := strings.TrimSuffix(r.file, filepath.Ext(r.file))
	switch a.form {
	case searchable:
		a.saveFile(base+" (searchable).pdf", func(path string) error { return copyFile(path, r.pdf) })
	case retyped:
		a.saveFile(base+" (retyped).pdf", func(path string) error { return copyFile(path, r.textPDF) })
	default:
		a.saveFile(base+".txt", func(path string) error {
			return os.WriteFile(path, []byte(pdfocr.Text(r.pages)), 0o644)
		})
	}
}

// saveFile asks where to save a file, and has write write it there.
func (a *app) saveFile(name string, write func(path string) error) {
	a.fx.spawn(func() {
		path, err := a.fx.saveAs(name)
		if err == nil && path != "" {
			err = write(path)
		}
		a.fx.update(func() {
			switch {
			case err != nil:
				a.toast = &toast{title: fmt.Sprintf("Couldn’t save “%s”", filepath.Base(name)), detail: err.Error()}
			case path != "":
				a.toast = &toast{title: fmt.Sprintf("Saved “%s”", filepath.Base(path)), saved: path}
			}
		})
	})
}

// syncMenu enables the menu bar's items that can be used now.
func (a *app) syncMenu() {
	open, save := a.canOpen(), a.phase == done
	if open != a.menuOpen || save != a.menuSave {
		a.menuOpen, a.menuSave = open, save
		a.fx.menu(open, save)
	}
}

// copyFile copies the file at src to dst.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// convertFile reads the PDF at path, and writes the PDFs it makes of it to
// a temporary directory, which is removed if it fails.
func convertFile(ctx context.Context, conv *pdfocr.Converter, path string, progress pdfocr.Progress) (_ *result, err error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	dir, err := os.MkdirTemp("", "unscan-")
	if err != nil {
		return nil, err
	}
	r := &result{dir: dir, pdf: filepath.Join(dir, "searchable.pdf"), textPDF: filepath.Join(dir, "text.pdf")}
	defer func() {
		if err != nil {
			r.remove()
		}
	}()
	pdf, err := os.Create(r.pdf)
	if err != nil {
		return nil, err
	}
	defer pdf.Close()
	textPDF, err := os.Create(r.textPDF)
	if err != nil {
		return nil, err
	}
	defer textPDF.Close()
	if r.pages, err = conv.Convert(ctx, in, pdfocr.Output{PDF: pdf, TextPDF: textPDF}, pdfocr.Options{}, progress); err != nil {
		return nil, err
	}
	if err := pdf.Close(); err != nil {
		return nil, err
	}
	if err := textPDF.Close(); err != nil {
		return nil, err
	}
	return r, nil
}

// loadEngine loads the models, PDFium and the text PDF's font from the
// app's resources.
func loadEngine() (*pdfocr.Converter, error) {
	res, err := mygo.App.Path(mygo.PathResources)
	if err != nil {
		return nil, err
	}
	paths := ocr.PathsIn(res)
	if err := paths.Check(); err != nil {
		return nil, fmt.Errorf("%w. Run `go run ./cmd/fetchdeps` in the project first.", err)
	}
	engine, err := ocr.New(paths, ocr.Options{})
	if err != nil {
		return nil, err
	}
	cache := ""
	if dir, err := mygo.App.Path(mygo.PathCache); err == nil {
		cache = filepath.Join(dir, "wazero")
	}
	conv, err := pdfocr.New(engine, cache)
	if err != nil {
		engine.Close()
		return nil, err
	}
	fonts, err := pdfocr.LoadTextFonts(res)
	if err == nil {
		err = conv.SetTextFonts(fonts)
	}
	if err != nil {
		conv.Close()
		engine.Close()
		return nil, fmt.Errorf("%w. Run `go run ./cmd/fetchdeps` in the project first.", err)
	}
	return conv, nil
}

func main() {
	a := &app{}
	var win *mygo.Window
	var launched []string // files the app was opened with before its window
	// Files opened with the app, as by `open -a Unscan scan.pdf`.
	mygo.App.OnOpenFile(func(path string) {
		if win == nil {
			launched = append(launched, path)
			return
		}
		win.Update(func() { a.drop([]string{path}) })
	})
	mygo.App.WhenReady(func() {
		do := func(fn func()) func(*mygo.MenuItem, *mygo.Window) {
			return func(*mygo.MenuItem, *mygo.Window) { win.Update(fn) }
		}
		menu := mygo.NewMenu([]*mygo.MenuItem{
			{Role: mygo.RoleAppMenu},
			{Label: "File", Submenu: []*mygo.MenuItem{
				{ID: "open", Label: "Open…", Accelerator: "CmdOrCtrl+O", Click: do(a.choose)},
				{ID: "save", Label: "Save…", Accelerator: "CmdOrCtrl+S", Disabled: true, Click: do(a.save)},
				mygo.Separator(),
				{Role: mygo.RoleClose},
			}},
			{Role: mygo.RoleEditMenu},
			{Label: "View", Submenu: []*mygo.MenuItem{{Role: mygo.RoleToggleFullScreen}}},
			{Role: mygo.RoleWindowMenu},
		})
		mygo.App.SetMenu(menu)
		pdfFilter := []mygo.FileFilter{{Name: "PDF", Extensions: []string{"pdf"}}}
		a.fx = effects{
			update: func(fn func()) { win.Update(fn) },
			spawn:  func(fn func()) { go fn() },
			open: func() (string, error) {
				paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: win, Title: "Open a Scanned PDF", Filters: pdfFilter})
				if err != nil || len(paths) == 0 {
					return "", err
				}
				return paths[0], nil
			},
			saveAs: func(name string) (string, error) {
				return mygo.Dialog.Save(mygo.SaveDialogOptions{Parent: win, DefaultPath: name, CreateDirectories: true})
			},
			reveal: mygo.Shell.ShowItemInFolder,
			menu: func(open, save bool) {
				menu.ItemByID("open").SetEnabled(open)
				menu.ItemByID("save").SetEnabled(save)
			},
			dock: func(progress float64) { win.SetProgressBar(mygo.ProgressBar{Value: max(progress, 0)}) },
			attend: func() {
				if !win.IsFocused() {
					win.FlashFrame(true)
				}
			},
		}
		win = mygo.NewWindow(mygo.WindowOptions{
			Title:          "Unscan",
			Width:          960,
			Height:         720,
			MinWidth:       760,
			MinHeight:      480,
			TitleBarStyle:  mygo.TitleBarHidden,
			TitleBarHeight: barHeight,
			// The window controls, 16 high, centered in the bar the view
			// draws (macOS).
			TrafficLightPosition: &mygo.Point{X: 20, Y: (barHeight - 16) / 2},
			StateKey:             "main",
			Content:              ui.View(a.view),
		})
		if len(launched) > 0 {
			win.Update(func() { a.drop(launched) })
		}
		go func() {
			conv, err := loadEngine()
			win.Update(func() {
				if err == nil {
					a.fx.convert = func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
						return convertFile(ctx, conv, path, p)
					}
					a.fx.render = func(path string, page int) (image.Image, error) {
						return conv.Preview(path, page, previewSize)
					}
				}
				a.loaded(err)
			})
		}()
	})
	mygo.App.OnQuit(func() { a.result.remove() })
	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
