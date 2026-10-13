package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"unscan/internal/pdfocr"
)

// fixture is the app with its effects run at once, a fake converter,
// previews of blank pages, and dialogs that answer with files in dir.
type fixture struct {
	*app
	dir, scan string
	rendered  []string // the previews rendered, as "path#page"
	revealed  []string
	menus     [][2]bool
	dock      []float64
}

func newFixture(t *testing.T, convert func(ctx context.Context, path string, p pdfocr.Progress) (*result, error)) *fixture {
	dir := t.TempDir()
	f := &fixture{app: &app{}, dir: dir, scan: filepath.Join(dir, "scan.pdf")}
	if err := os.WriteFile(f.scan, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	var suggested []string
	t.Cleanup(func() {
		// The save dialog starts beside the PDF read.
		for _, name := range suggested {
			if filepath.Dir(name) != dir {
				t.Errorf("save dialog suggested %q, not beside %q", name, f.scan)
			}
		}
	})
	f.fx = effects{
		update:  func(fn func()) { fn() },
		spawn:   func(fn func()) { fn() },
		convert: convert,
		render: func(path string, page int) (image.Image, error) {
			f.rendered = append(f.rendered, fmt.Sprintf("%s#%d", path, page))
			return image.NewRGBA(image.Rect(0, 0, 85, 110)), nil
		},
		open: func() (string, error) { return f.scan, nil },
		saveAs: func(name string) (string, error) {
			suggested = append(suggested, name)
			return name, nil
		},
		reveal: func(path string) { f.revealed = append(f.revealed, path) },
		menu:   func(open, save bool) { f.menus = append(f.menus, [2]bool{open, save}) },
		dock:   func(p float64) { f.dock = append(f.dock, p) },
		attend: func() {},
	}
	return f
}

// tempFile writes data to a file of its own, as a conversion does.
func tempFile(t *testing.T, data string) string {
	path := filepath.Join(t.TempDir(), "out.pdf")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func click(t *testing.T, tt *ui.Tester, label string) {
	t.Helper()
	if err := tt.Click(label); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
}

func wantTexts(t *testing.T, tt *ui.Tester, texts ...string) {
	t.Helper()
	for _, want := range texts {
		if !tt.HasText(want) {
			t.Errorf("no %q in %q", want, tt.Texts())
		}
	}
}

func TestConvertFlow(t *testing.T) {
	pages := []pdfocr.Page{
		{Index: 0, Text: "Quarterly report", Lines: 1},
		{Index: 1, Text: "扫描文档", Lines: 1},
		{Index: 2, Text: "Born digital", Skipped: true},
	}
	var progress []int
	res := &result{pdf: tempFile(t, "%PDF searchable"), textPDF: tempFile(t, "%PDF text")}
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		for i := range len(pages) + 1 {
			p(i, len(pages))
			progress = append(progress, i)
		}
		res.pages = pages
		return res, nil
	})
	tt := ui.NewTester(f.view, 960, 720)
	wantTexts(t, tt, "Drop a scanned PDF here")

	click(t, tt, "Choose PDF…")
	if f.phase != done || len(progress) != 4 {
		t.Fatalf("phase %v, progress %v, error %q", f.phase, progress, f.err)
	}
	wantTexts(t, tt, "scan.pdf", "1 of 3", "Looks just like the scan, with text you can search, select and copy.")
	// The page shown, then the next, ready to turn to.
	if want := []string{res.pdf + "#0", res.pdf + "#1"}; !slices.Equal(f.rendered, want) {
		t.Errorf("rendered %q, not %q", f.rendered, want)
	}

	click(t, tt, "Next Page")
	wantTexts(t, tt, "2 of 3")
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	wantTexts(t, tt, "3 of 3", "This page already had text, so it was left as it was.")
	tt.Key(0, ui.KeyRight) // past the end
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	wantTexts(t, tt, "2 of 3")

	click(t, tt, "Retyped PDF")
	wantTexts(t, tt, "Retyped in real fonts on white paper. Misread characters show here.")
	if !slices.Contains(f.rendered, res.textPDF+"#1") {
		t.Errorf("rendered %q, not the retyped page", f.rendered)
	}
	// After a click on the form, the arrows still turn the page.
	for _, key := range []ui.Key{ui.KeyLeft, ui.KeyRight} {
		tt.Key(0, key)
		tt.Frame()
	}
	if f.form != retyped || f.at != 1 {
		t.Errorf("the arrows chose form %d and page %d", f.form, f.at+1)
	}
	click(t, tt, "Text")
	wantTexts(t, tt, "扫描文档")
	click(t, tt, "Copy All")
	if got := tt.Clipboard(); got != pdfocr.Text(pages) {
		t.Errorf("copied %q", got)
	}

	// Save saves the form shown.
	for _, s := range []struct{ form, file, want string }{
		{"Text", "scan.txt", pdfocr.Text(pages)},
		{"Searchable PDF", "scan (searchable).pdf", "%PDF searchable"},
		{"Retyped PDF", "scan (retyped).pdf", "%PDF text"},
	} {
		click(t, tt, s.form)
		click(t, tt, "Save…")
		path := filepath.Join(f.dir, s.file)
		if got, _ := os.ReadFile(path); string(got) != s.want {
			t.Errorf("saved %q in %s", got, s.file)
		}
		wantTexts(t, tt, "Saved “"+s.file+"”")
		f.revealed = nil
		click(t, tt, revealLabel())
		if !slices.Equal(f.revealed, []string{path}) {
			t.Errorf("revealed %q, not %q", f.revealed, path)
		}
	}
}

// TestWaitsForModels reads a PDF chosen while the models load once they
// have.
func TestWaitsForModels(t *testing.T) {
	f := newFixture(t, nil)
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	wantTexts(t, tt, "Getting ready…")
	f.fx.convert = func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: []pdfocr.Page{{Text: "Hello", Lines: 1}}, pdf: tempFile(t, "%PDF")}, nil
	}
	f.loaded(nil)
	tt.Frame()
	if f.phase != done || f.result.file != f.scan {
		t.Errorf("phase %v, error %q", f.phase, f.err)
	}
}

func TestModelsFail(t *testing.T) {
	f := newFixture(t, nil)
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	f.loaded(errors.New("no models here"))
	tt.Frame()
	wantTexts(t, tt, "Text recognition didn’t start", "no models here")
	if f.phase != idle || tt.HasText("Choose PDF…") {
		t.Errorf("phase %v, texts %q", f.phase, tt.Texts())
	}
	if last := f.menus[len(f.menus)-1]; last != [2]bool{false, false} {
		t.Errorf("menu items enabled: %v", last)
	}
}

// TestQueuedUpdates runs updates after the background work returns, as
// the window does, so that the result outlives the run's deferred cleanup.
func TestQueuedUpdates(t *testing.T) {
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: []pdfocr.Page{{Text: "Hello", Lines: 1}}, pdf: tempFile(t, "%PDF searchable")}, nil
	})
	var queue []func()
	f.fx.update = func(fn func()) { queue = append(queue, fn) }
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		fn()
		tt.Frame()
	}
	if f.phase != done || !tt.HasText("Save…") || tt.HasText("1 of 1") {
		t.Errorf("phase %v, error %q, texts %q", f.phase, f.err, tt.Texts())
	}
	if len(f.rendered) != 1 {
		t.Errorf("rendered %q", f.rendered)
	}
}

func TestConvertError(t *testing.T) {
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return nil, errors.New("not a PDF")
	})
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	if f.phase != idle || !tt.HasText("Couldn’t read “scan.pdf”: not a PDF") {
		t.Errorf("phase %v, texts %q", f.phase, tt.Texts())
	}
}

// TestCancel stops reading at once: the document shown before shows
// again, and the canceled run's files are removed as it ends.
func TestCancel(t *testing.T) {
	var dirs []string
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		return &result{pages: []pdfocr.Page{{Text: "Hello"}}, pdf: filepath.Join(dir, "searchable.pdf"), dir: dir}, nil
	})
	var spawned []func()
	f.fx.spawn = func(fn func()) { spawned = append(spawned, fn) }
	step := func() { // runs what was sent to the background first
		fn := spawned[0]
		spawned = spawned[1:]
		fn()
	}
	tt := ui.NewTester(f.view, 960, 720)
	for i, want := range []phase{idle, done} {
		f.choose()
		step() // the dialog
		tt.Frame()
		wantTexts(t, tt, "Opening…")
		click(t, tt, "Cancel")
		if f.phase != want {
			t.Errorf("canceled run %d: phase %v, not %v", i+1, f.phase, want)
		}
		step() // the reading, which ends
		if _, err := os.Stat(dirs[len(dirs)-1]); !os.IsNotExist(err) {
			t.Errorf("canceled run %d kept its files: %v", i+1, err)
		}
		if i == 0 {
			// Read one to the end, and show it, to cancel the next over it.
			f.choose()
			for len(spawned) > 0 {
				step()
			}
			tt.Frame()
			for len(spawned) > 0 {
				step() // its preview
			}
		}
	}
	if f.dock[len(f.dock)-1] >= 0 {
		t.Errorf("the Dock shows progress after canceling: %v", f.dock)
	}
}

// TestResultRemoved reads a PDF twice: the first result's files are
// removed when the second replaces it, and the second's as on quitting.
func TestResultRemoved(t *testing.T) {
	var dirs []string
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		dir := t.TempDir()
		dirs = append(dirs, dir)
		return &result{pages: []pdfocr.Page{{Text: "Hello"}}, pdf: filepath.Join(dir, "searchable.pdf"), dir: dir}, nil
	})
	tt := ui.NewTester(f.view, 960, 720)
	f.choose()
	tt.Frame()
	f.choose()
	tt.Frame()
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Errorf("the first result's files are kept: %v", err)
	}
	if _, err := os.Stat(dirs[1]); err != nil {
		t.Errorf("the second result's files are gone: %v", err)
	}
	f.result.remove()
	if _, err := os.Stat(dirs[1]); !os.IsNotExist(err) {
		t.Errorf("the second result's files are kept: %v", err)
	}
}

// TestDrop reads the first PDF of the files dropped, and says so when none
// is one.
func TestDrop(t *testing.T) {
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: []pdfocr.Page{{Text: "Hello"}}, pdf: tempFile(t, "%PDF")}, nil
	})
	tt := ui.NewTester(f.view, 960, 720)
	f.drop([]string{filepath.Join(f.dir, "photo.jpg")})
	tt.Frame()
	wantTexts(t, tt, "“photo.jpg” isn’t a PDF")
	f.drop([]string{filepath.Join(f.dir, "photo.jpg"), f.scan})
	tt.Frame()
	if f.phase != done || f.result.file != f.scan {
		t.Errorf("phase %v, error %q", f.phase, f.err)
	}
}

// TestMenu enables Open while a PDF can be opened, and Save while one is
// shown.
func TestMenu(t *testing.T) {
	f := newFixture(t, nil)
	tt := ui.NewTester(f.view, 960, 720)
	f.fx.spawn = func(func()) {} // reading never ends
	f.fx.convert = func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) { return nil, nil }
	f.start(f.scan)
	tt.Frame()
	f.stop()
	f.result = &result{file: f.scan, pages: []pdfocr.Page{{Text: "Hello"}}}
	f.phase = done
	tt.Frame()
	want := [][2]bool{{true, false}, {false, false}, {true, true}}
	if !slices.Equal(f.menus, want) {
		t.Errorf("menu items enabled %v, not %v", f.menus, want)
	}
}

func TestPreviewError(t *testing.T) {
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: []pdfocr.Page{{Text: "Hello"}}, pdf: tempFile(t, "%PDF")}, nil
	})
	f.fx.render = func(string, int) (image.Image, error) { return nil, errors.New("damaged") }
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	tt.Frame()
	wantTexts(t, tt, "This page can’t be shown: damaged")
}

func TestTimeLeft(t *testing.T) {
	for _, c := range []struct {
		spent       time.Duration
		done, pages int
		want        string
	}{
		{10 * time.Second, 1, 100, ""}, // too few pages to tell
		{2 * time.Second, 4, 100, ""},  // too soon to tell
		{10 * time.Second, 10, 12, "Less than a minute left"},
		{10 * time.Second, 10, 70, "About a minute left"},
		{20 * time.Second, 10, 100, "About 3 minutes left"},
	} {
		if got := timeLeft(c.spent, c.done, c.pages); got != c.want {
			t.Errorf("timeLeft(%v, %d, %d) = %q, not %q", c.spent, c.done, c.pages, got, c.want)
		}
	}
}
