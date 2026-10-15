package main

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
	menus     []menuState
	dock      []float64
}

func newFixture(t *testing.T, convert func(ctx context.Context, path string, p pdfocr.Progress) (*result, error)) *fixture {
	dir := t.TempDir()
	f := &fixture{app: &app{sidebar: true}, dir: dir, scan: filepath.Join(dir, "scan.pdf")}
	if err := os.WriteFile(f.scan, []byte("%PDF-1.4"), 0o600); err != nil {
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
		open: func() ([]string, error) { return []string{f.scan}, nil },
		saveAs: func(name string) (string, error) {
			suggested = append(suggested, name)
			return name, nil
		},
		reveal:       func(path string) { f.revealed = append(f.revealed, path) },
		menu:         func(m menuState) { f.menus = append(f.menus, m) },
		dock:         func(p float64) { f.dock = append(f.dock, p) },
		attend:       func() {},
		chooseFont:   func() (string, error) { return "", nil },
		saveFonts:    func(fontChoices) error { return nil },
		setUpdates:   func(updatePrefs) {},
		checkUpdates: func() {},
	}
	return f
}

// tempFile writes data to a file of its own, as a conversion does.
func tempFile(t *testing.T, data string) string {
	path := filepath.Join(t.TempDir(), "out.pdf")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
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
		if got, _ := os.ReadFile(path); string(got) != s.want { // #nosec G304 -- in the test's directory
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
	if last := f.menus[len(f.menus)-1]; last.open || last.save {
		t.Errorf("menu items enabled: %+v", last)
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

// TestResultKept reads a PDF twice: both results stay in the history,
// newest first, with their files.
func TestResultKept(t *testing.T) {
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
	for i, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("result %d's files are gone: %v", i+1, err)
		}
	}
	if len(f.history) != 2 || f.history[0].dir != dirs[1] || f.history[1].dir != dirs[0] || f.result != f.history[0] {
		t.Errorf("history %+v, showing %+v", f.history, f.result)
	}
}

// TestHistory reads PDFs, which the sidebar lists, newest first: one
// chosen there shows in the form shown, from its first page. Those removed, from their rows' menus or with
// Delete, go with their files, and one shown gives its place to the next;
// clearing the history removes the rest.
func TestHistory(t *testing.T) {
	dirs := map[string]string{}
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		dir := t.TempDir()
		dirs[filepath.Base(path)] = dir
		pages := []pdfocr.Page{{Text: "One", Lines: 1}, {Text: "Two", Lines: 1}}
		return &result{pages: pages, pdf: tempFile(t, "%PDF"), textPDF: tempFile(t, "%PDF text"), dir: dir, size: 2_500_000}, nil
	})
	opened := []string{f.scan, filepath.Join(f.dir, "report.pdf"), filepath.Join(f.dir, "notes.pdf"), filepath.Join(f.dir, "memo.pdf")}
	f.fx.open = func() ([]string, error) {
		path := opened[0]
		opened = opened[1:]
		return []string{path}, nil
	}
	gone := func(name string) bool {
		_, err := os.Stat(dirs[name])
		return os.IsNotExist(err)
	}
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	click(t, tt, "Retyped PDF")
	click(t, tt, "Next Page")
	for range 2 {
		f.choose()
		tt.Frame()
	}
	wantTexts(t, tt, "scan.pdf", "report.pdf", "notes.pdf", "3 documents · 7.5 MB")
	if !slices.ContainsFunc(tt.Texts(), func(s string) bool { return strings.HasPrefix(s, "2 pages · Today, ") }) {
		t.Errorf("no row says when its PDF was read in %q", tt.Texts())
	}
	if f.result.file != filepath.Join(f.dir, "notes.pdf") || f.form != retyped || f.at != 0 {
		t.Errorf("showing %s in form %d at page %d", f.result.file, f.form, f.at+1)
	}

	// The oldest, chosen in the sidebar.
	click(t, tt, "scan.pdf")
	if f.result != f.history[2] || f.form != retyped || f.at != 0 {
		t.Errorf("showing %s in form %d at page %d", f.result.file, f.form, f.at+1)
	}

	// Delete asks to remove the one chosen, which Cancel keeps.
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	wantTexts(t, tt, "Remove “scan.pdf” from history?")
	click(t, tt, "Cancel")
	if len(f.history) != 3 || gone("scan.pdf") {
		t.Fatalf("canceling removed %+v", f.history)
	}

	// One not shown, removed from its row's menu.
	if err := tt.RightClick("report.pdf"); err != nil {
		t.Fatal(err)
	}
	if want := []string{revealScanLabel(), "-", "Remove from History…"}; !slices.Equal(tt.Menu(), want) {
		t.Errorf("menu %q, not %q", tt.Menu(), want)
	}
	if err := tt.ChooseMenuItem("Remove from History…"); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	wantTexts(t, tt, "Remove “report.pdf” from history?")
	click(t, tt, "Remove")
	if len(f.history) != 2 || !gone("report.pdf") || gone("scan.pdf") || f.result != f.history[1] {
		t.Errorf("history %d long, showing %+v", len(f.history), f.result)
	}

	// The one shown, removed, gives its place to the one below it.
	click(t, tt, "notes.pdf")
	tt.Key(0, ui.KeyBackspace)
	tt.Frame()
	click(t, tt, "Remove")
	if len(f.history) != 1 || !gone("notes.pdf") || f.result != f.history[0] || f.phase != done {
		t.Errorf("history %d long, showing %+v", len(f.history), f.result)
	}

	f.choose()
	tt.Frame()
	click(t, tt, "Clear…")
	wantTexts(t, tt, "Clear history?", "The searchable and retyped PDFs of 2 documents (5.0 MB) will be deleted. The scans themselves aren’t touched.")
	click(t, tt, "Clear History")
	if len(f.history) != 0 || !gone("scan.pdf") || !gone("memo.pdf") || f.result != nil || f.phase != idle {
		t.Errorf("history %+v, phase %v after clearing", f.history, f.phase)
	}
	if tt.HasText("Clear…") || !tt.HasText("Drop a scanned PDF here") {
		t.Errorf("texts %q after clearing", tt.Texts())
	}
}

// TestHistoryOnDisk reads the history from folders, as convertFile writes
// them: those of readings that did not end are left out, and removed once
// they are a day old.
func TestHistoryOnDisk(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	var saved []*result
	for i, file := range []string{"/scans/a.pdf", "/scans/b.pdf"} {
		r := &result{
			file: file, read: now.Add(time.Duration(i-1) * time.Hour),
			pages: []pdfocr.Page{{Index: 0, Text: "Hello", Lines: 1}, {Index: 1, Text: "Born digital", Skipped: true}},
			dir:   filepath.Join(dir, fmt.Sprintf("read-%d", i)),
		}
		r.pdf, r.textPDF = filepath.Join(r.dir, "searchable.pdf"), filepath.Join(r.dir, "text.pdf")
		if err := os.Mkdir(r.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{r.pdf, r.textPDF} {
			if err := os.WriteFile(path, []byte("%PDF "+file), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.save(); err != nil {
			t.Fatal(err)
		}
		r.size = 2*int64(len("%PDF "+file)) + fileSize(t, filepath.Join(r.dir, resultFile))
		saved = append(saved, r)
	}
	unfinished := func(name, file string, age time.Duration) string {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, file), []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return path
	}
	reading := unfinished("read-reading", "searchable.pdf", time.Minute)
	crashed := unfinished("read-crashed", "searchable.pdf", 48*time.Hour)
	damaged := unfinished("read-damaged", resultFile, time.Minute)

	got := loadHistory(dir, now)
	want := []*result{saved[1], saved[0]}
	if len(got) != len(want) {
		t.Fatalf("read %d results, want %d", len(got), len(want))
	}
	for i, r := range got {
		if !r.read.Equal(want[i].read) {
			t.Errorf("result %d read at %v, want %v", i, r.read, want[i].read)
		}
		r.read = want[i].read
		if !reflect.DeepEqual(r, want[i]) {
			t.Errorf("result %d is %+v, want %+v", i, r, want[i])
		}
	}
	for _, path := range []string{reading, damaged} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("removed %s, which is new: %v", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(crashed); !os.IsNotExist(err) {
		t.Errorf("kept %s, which is old: %v", filepath.Base(crashed), err)
	}
	if got := loadHistory(filepath.Join(dir, "missing"), now); got != nil {
		t.Errorf("read %+v from no folder", got)
	}
}

func fileSize(t *testing.T, path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

// TestCompare shows the original beside the retyped PDF and the text, and
// the searchable PDF, which looks just like it, alone. Both turn together,
// and the split stays where it is dragged.
func TestCompare(t *testing.T) {
	pages := []pdfocr.Page{{Text: "第一页", Lines: 1}, {Text: "Second page", Lines: 1}}
	res := &result{pdf: tempFile(t, "%PDF searchable"), textPDF: tempFile(t, "%PDF text")}
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		res.pages = pages
		return res, nil
	})
	tt := ui.NewTester(f.view, 1200, 800)
	click(t, tt, "Choose PDF…")
	f.toggleCompare()
	tt.Frame()
	if slices.Contains(tt.Texts(), "Original") {
		t.Errorf("the searchable PDF shows beside the original: %q", tt.Texts())
	}
	if m := f.menus[len(f.menus)-1]; !m.compare || m.canCompare {
		t.Errorf("menu %+v on the searchable PDF", m)
	}

	click(t, tt, "Retyped PDF")
	wantCaptions(t, tt, "Original", "Retyped PDF")
	for _, want := range []string{res.textPDF + "#0", res.textPDF + "#1"} {
		if !slices.Contains(f.rendered, want) {
			t.Errorf("rendered %q, not %s", f.rendered, want)
		}
	}
	if m := f.menus[len(f.menus)-1]; !m.compare || !m.canCompare {
		t.Errorf("menu %+v comparing the retyped PDF", m)
	}
	click(t, tt, "Next Page")
	wantCaptions(t, tt, "Original", "Retyped PDF")
	wantTexts(t, tt, "2 of 2")
	if f.at != 1 {
		t.Errorf("at page %d", f.at+1)
	}

	// Dragged, the split stays where it was left.
	div, ok := tt.Find("Divider")
	if !ok {
		t.Fatalf("no divider in %q", tt.Texts())
	}
	x, y := div.X+div.W/2, div.Y+div.H/2
	tt.Press(x, y)
	tt.Move(x+120, y)
	tt.Release(x+120, y)
	tt.Frame()
	if f.split <= 0.5 || f.split >= 0.75 {
		t.Errorf("split %v after a drag to the right", f.split)
	}
	// The arrows go on turning the page, as the drag leaves the keyboard
	// focus where it was; from Tab, they move the divider.
	dragged := f.split
	tt.Key(0, ui.KeyLeft)
	tt.Frame()
	if f.at != 0 || f.split != dragged {
		t.Errorf("after a drag, Left shows page %d, split %v", f.at+1, f.split)
	}
	moved := false
	for range 20 {
		tt.Key(0, ui.KeyTab)
		tt.Frame()
		tt.Key(0, ui.KeyRight)
		tt.Frame()
		if moved = f.split > dragged; moved {
			break
		}
	}
	if !moved {
		t.Errorf("Tab and Right never moved the divider: split %v", f.split)
	}
	f.turn(1)
	tt.Frame()

	click(t, tt, "Text")
	wantCaptions(t, tt, "Original", "Text")
	wantTexts(t, tt, "Second page")
	if !slices.Contains(f.rendered, res.pdf+"#1") {
		t.Errorf("rendered %q, not the original", f.rendered)
	}

	click(t, tt, "Compare with Original")
	if f.comparing || slices.Contains(tt.Texts(), "Original") {
		t.Errorf("comparing %v, texts %q", f.comparing, tt.Texts())
	}
}

// wantCaptions checks that the panes compared show under their names: the
// form's shows in its picker too.
func wantCaptions(t *testing.T, tt *ui.Tester, left, right string) {
	t.Helper()
	texts := tt.Texts()
	n := 0
	for _, s := range texts {
		if s == right {
			n++
		}
	}
	if !slices.Contains(texts, left) || n < 2 {
		t.Errorf("no panes %q and %q in %q", left, right, texts)
	}
}

func TestWhen(t *testing.T) {
	now := time.Date(2026, 10, 13, 14, 5, 0, 0, time.Local)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{now.Add(-time.Hour), "Today, 13:05"},
		{time.Date(2026, 10, 12, 9, 12, 0, 0, time.Local), "Yesterday, 09:12"},
		{time.Date(2026, 10, 2, 23, 0, 0, 0, time.Local), "Oct 2"},
		{time.Date(2025, 12, 31, 8, 0, 0, 0, time.Local), "Dec 31, 2025"},
	} {
		if got := when(c.t, now); got != c.want {
			t.Errorf("when(%v) = %q, not %q", c.t, got, c.want)
		}
	}
}

func TestByteSize(t *testing.T) {
	for _, c := range []struct {
		n    int64
		want string
	}{
		{0, "0 bytes"},
		{1, "1 byte"},
		{999, "999 bytes"},
		{1000, "1.0 KB"},
		{48_200, "48 KB"},
		{999_600, "1.0 MB"},
		{412_000_000, "412 MB"},
		{1_234_567_890, "1.2 GB"},
	} {
		if got := byteSize(c.n); got != c.want {
			t.Errorf("byteSize(%d) = %q, not %q", c.n, got, c.want)
		}
	}
}

// TestDrop reads the PDFs of the files dropped, and says which files are
// not PDFs.
func TestDrop(t *testing.T) {
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: []pdfocr.Page{{Text: "Hello"}}, pdf: tempFile(t, "%PDF")}, nil
	})
	tt := ui.NewTester(f.view, 960, 720)
	f.drop([]string{filepath.Join(f.dir, "photo.jpg")})
	tt.Frame()
	wantTexts(t, tt, "“photo.jpg” isn’t a PDF")
	report := filepath.Join(f.dir, "report.PDF")
	f.drop([]string{filepath.Join(f.dir, "photo.jpg"), f.scan, report})
	tt.Frame()
	wantTexts(t, tt, "“photo.jpg” isn’t a PDF")
	if f.phase != done || len(f.history) != 2 || f.history[0].file != report || f.history[1].file != f.scan {
		t.Errorf("phase %v, error %q, history %+v", f.phase, f.err, f.history)
	}
	f.drop([]string{filepath.Join(f.dir, "a.txt"), filepath.Join(f.dir, "b.png")})
	tt.Frame()
	wantTexts(t, tt, "2 files aren’t PDFs")
}

// TestMenu enables Open while PDFs can be opened, as they can while one
// is read, and Save while one is shown.
func TestMenu(t *testing.T) {
	f := newFixture(t, nil)
	tt := ui.NewTester(f.view, 960, 720)
	f.fx.spawn = func(func()) {} // reading never ends
	f.fx.convert = func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) { return nil, nil }
	f.add(f.scan)
	tt.Frame()
	f.stop()
	f.result = &result{file: f.scan, pages: []pdfocr.Page{{Text: "Hello"}}}
	f.phase = done
	tt.Frame()
	var got [][2]bool
	for _, m := range f.menus {
		got = append(got, [2]bool{m.open, m.save})
	}
	want := [][2]bool{{true, false}, {true, true}}
	if !slices.Equal(got, want) {
		t.Errorf("menu items enabled %v, not %v", got, want)
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

// fakeFonts reads font files by their names: Songti.ttc holds Songti SC
// Black, Bold and Regular, Arial.ttf one font, and others none it can use.
func fakeFonts(fonts fontChoices) fontChoices {
	for i := range fonts {
		f := &fonts[i]
		f.names, f.name, f.err = nil, "", ""
		if f.Path == "" {
			continue
		}
		switch filepath.Base(f.Path) {
		case "Songti.ttc":
			f.names = []string{"Songti SC Black", "Songti SC Bold", "Songti SC Regular"}
		case "Arial.ttf":
			f.names = []string{"Arial"}
		default:
			f.err = "Not a TrueType font (.ttf or .ttc)."
			continue
		}
		if f.Index < 0 {
			f.Index = pdfocr.PickFont(f.names, fontFaces[i].bold)
		}
		f.name = f.names[f.Index]
	}
	return fonts
}

// TestFonts chooses fonts in the Settings dialog: a collection's regular one,
// then another of it from its menu, a font file of one font, one that
// cannot be used, and the built-in font again, saving each choice. Fonts
// chosen before the models load are used once they have.
func TestFonts(t *testing.T) {
	f := newFixture(t, nil)
	var saved []fontChoices
	f.fx.saveFonts = func(fonts fontChoices) error {
		saved = append(saved, fonts)
		return nil
	}
	var chosen string
	f.fx.chooseFont = func() (string, error) { return chosen, nil }
	tt := ui.NewTester(f.view, 960, 720)
	f.openSettings()
	tt.Frame()
	wantTexts(t, tt, "Fonts", "Serif bold", "Noto Serif SC Bold", "LXGW WenKai", "Built in")

	// Before the models load, a font file chosen shows by its name.
	chosen = "/Library/Fonts/Songti.ttc"
	click(t, tt, "Choose…")
	wantTexts(t, tt, "Songti.ttc")
	if f.fonts[0].Path != chosen || f.fonts[0].Index != -1 || len(saved) != 1 || saved[0][0].Path != chosen {
		t.Fatalf("fonts %+v, saved %+v", f.fonts[0], saved)
	}
	var used []fontChoices
	f.fx.useFonts = func(fonts fontChoices) fontChoices {
		used = append(used, fonts)
		return fakeFonts(fonts)
	}
	f.loaded(nil)
	tt.Frame()
	if len(used) != 1 || used[0][0].Path != chosen {
		t.Fatalf("used %+v once the models loaded", used)
	}
	wantTexts(t, tt, "Songti SC Regular", "Songti.ttc")
	if f.fonts[0].Index != 2 {
		t.Errorf("index %d, want Songti SC Regular's", f.fonts[0].Index)
	}

	// Another font of the collection, from its menu.
	click(t, tt, "Songti SC Regular")
	click(t, tt, "Songti SC Black")
	tt.Frame()
	if f.fonts[0].Index != 0 || len(saved) != 2 || saved[1][0].Index != 0 || len(used) != 2 {
		t.Errorf("index %d, saved %d times, used %d times", f.fonts[0].Index, len(saved), len(used))
	}

	// A font file for Kai, and one that cannot be used for sans-serif.
	chosen = "/Library/Fonts/Arial.ttf"
	f.chooseFont(4)
	chosen = "/Library/Fonts/Hiragino.otf"
	f.chooseFont(2)
	tt.Frame()
	wantTexts(t, tt, "Arial", "Arial.ttf", "Not a TrueType font (.ttf or .ttc).")

	// The built-in font again.
	click(t, tt, "Reset")
	if !reflect.DeepEqual(f.fonts[0], fontChoice{}) || saved[len(saved)-1][0].Path != "" {
		t.Errorf("serif %+v after Reset", f.fonts[0])
	}
	wantTexts(t, tt, "Noto Serif SC")
	click(t, tt, "Done")
	if f.settingsOpen || tt.HasText("Serif bold") {
		t.Error("the dialog stays open")
	}
}

// TestFontsQueued uses fonts chosen while others are being read once they
// have been, and only then.
func TestFontsQueued(t *testing.T) {
	f := newFixture(t, nil)
	var pending []func()
	f.fx.spawn = func(fn func()) { pending = append(pending, fn) }
	var used []string
	f.fx.useFonts = func(fonts fontChoices) fontChoices {
		used = append(used, fonts[0].Path)
		return fakeFonts(fonts)
	}
	f.fonts[0] = fontChoice{Path: "/a/Songti.ttc", Index: -1}
	f.fontsChanged()
	f.fonts[0] = fontChoice{Path: "/b/Arial.ttf", Index: -1}
	f.fontsChanged()
	for len(pending) > 0 {
		fn := pending[0]
		pending = pending[1:]
		fn()
	}
	if want := []string{"/a/Songti.ttc", "/b/Arial.ttf"}; !slices.Equal(used, want) {
		t.Errorf("used %q, want %q", used, want)
	}
	if f.fonts[0].Path != "/b/Arial.ttf" || f.fonts[0].name != "Arial" || f.usingFonts {
		t.Errorf("fonts %+v", f.fonts[0])
	}
}

// TestSavedFonts saves the fonts chosen and reads them back, but for what
// is learned of their files; a damaged file chooses none.
func TestSavedFonts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fonts.json")
	var fonts fontChoices
	fonts[1] = fontChoice{Path: "/Library/Fonts/Songti.ttc", Index: 1, names: []string{"a", "b"}, name: "b"}
	fonts[4] = fontChoice{Path: "/Library/Fonts/Kai.ttf", Index: -1}
	if err := saveFonts(path, fonts); err != nil {
		t.Fatal(err)
	}
	got := loadFonts(path)
	fonts[1].names, fonts[1].name = nil, ""
	if !reflect.DeepEqual(got, fonts) {
		t.Errorf("read %+v, want %+v", got, fonts)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadFonts(path); !reflect.DeepEqual(got, fontChoices{}) {
		t.Errorf("read %+v from a damaged file", got)
	}
}

// TestUpdates changes in Settings whether the app checks for updates and
// installs them by itself, which only checking for them allows, and checks
// now. A copy that cannot update itself says so, and only checks.
func TestUpdates(t *testing.T) {
	f := newFixture(t, nil)
	var set []updatePrefs
	f.fx.setUpdates = func(u updatePrefs) { set = append(set, u) }
	checks := 0
	f.fx.checkUpdates = func() { checks++ }
	last := time.Date(2025, 3, 4, 10, 0, 0, 0, time.Local)
	f.updates = updatePrefs{enabled: true, checks: true, lastCheck: last}
	tt := ui.NewTester(f.view, 960, 720)
	f.openSettings()
	tt.Frame()
	wantTexts(t, tt, "Settings", "Updates", "Last checked: Mar 4, 2025")
	// Below the fonts, which a window this short scrolls.
	r, ok := tt.Find("Fonts")
	if !ok {
		t.Fatal("no fonts in Settings")
	}
	tt.Scroll(r.X+r.W/2, r.Y+r.H/2, 0, 1000)
	tt.Frame()

	click(t, tt, "Download and install updates automatically")
	click(t, tt, "Check for updates automatically")
	want := []updatePrefs{
		{enabled: true, checks: true, downloads: true, lastCheck: last},
		{enabled: true, checks: false, downloads: true, lastCheck: last},
	}
	if !slices.Equal(set, want) {
		t.Fatalf("set %+v, want %+v", set, want)
	}
	// Installing without asking needs checking without asking.
	click(t, tt, "Download and install updates automatically")
	if len(set) != 2 || !f.updates.downloads {
		t.Errorf("set %+v with checks off", set)
	}
	click(t, tt, "Check Now")
	if checks != 1 {
		t.Errorf("checked %d times", checks)
	}

	f.updates = updatePrefs{checks: true}
	tt.Frame()
	wantTexts(t, tt, "This copy of Unscan can’t update itself: it’s a development build, or it’s in a folder it can’t change.")
	click(t, tt, "Check for updates automatically")
	click(t, tt, "Check Now")
	if len(set) != 2 || !f.updates.checks || checks != 2 {
		t.Errorf("set %+v, checked %d times, where the app can't update itself", set, checks)
	}
	if tt.HasText("Not checked yet") {
		t.Error("says when it checked, where it can't update itself")
	}
}

func TestFontError(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&os.PathError{Op: "open", Path: "/x.ttf", Err: os.ErrNotExist}, "The file isn’t there anymore."},
		{&os.PathError{Op: "open", Path: "/x.ttf", Err: os.ErrPermission}, "Permission denied."},
		{errors.New("not a TrueType font (.ttf or .ttc)"), "Not a TrueType font (.ttf or .ttc)."},
	} {
		if got := fontError(c.err); got != c.want {
			t.Errorf("fontError(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// spawnLater keeps what is sent to the background until the test runs it.
type spawnLater struct{ fns []func() }

func (s *spawnLater) spawn(fn func()) { s.fns = append(s.fns, fn) }

// step runs what was sent first, and reports whether there was any.
func (s *spawnLater) step() bool {
	if len(s.fns) == 0 {
		return false
	}
	fn := s.fns[0]
	s.fns = s.fns[1:]
	fn()
	return true
}

// readFake reads any PDF as a page of its name, in a file of its own.
func readFake(t *testing.T) func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
	return func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		p(0, 1)
		p(1, 1)
		dir := t.TempDir()
		return &result{pages: []pdfocr.Page{{Text: filepath.Base(path), Lines: 1}}, pdf: tempFile(t, "%PDF"), dir: dir}, nil
	}
}

// TestQueue reads the PDFs dropped in turn. The first shows its progress
// and, once read, itself, while the next is read; the others wait in the
// sidebar, where one is taken out. One waiting already is not added again,
// and the window draws the user back once, as the last is read.
func TestQueue(t *testing.T) {
	f := newFixture(t, readFake(t))
	later := &spawnLater{}
	f.fx.spawn = later.spawn
	attended := 0
	f.fx.attend = func() { attended++ }
	a, b, c := filepath.Join(f.dir, "a.pdf"), filepath.Join(f.dir, "b.pdf"), filepath.Join(f.dir, "c.pdf")
	tt := ui.NewTester(f.view, 960, 720)
	f.drop([]string{a, b, c})
	f.add(b)
	tt.Frame()
	if f.phase != working || f.file != a || !slices.Equal(f.queue, []string{b, c}) {
		t.Fatalf("phase %v, reading %q, queue %q", f.phase, f.file, f.queue)
	}
	wantTexts(t, tt, "1 of 3 PDFs", "Cancel All", "b.pdf", "c.pdf", "Waiting")

	later.step() // a is read
	tt.Frame()
	if f.phase != done || f.result.file != a || f.file != b || len(f.history) != 1 {
		t.Fatalf("phase %v, showing %+v, reading %q", f.phase, f.result, f.file)
	}
	if got := f.dock[len(f.dock)-1]; got < 0.3 || got > 0.4 {
		t.Errorf("the Dock shows %v once a is read, not 1/3", got)
	}
	click(t, tt, "Remove from Queue")
	if len(f.queue) != 0 || tt.HasText("Waiting") {
		t.Errorf("queue %q, texts %q", f.queue, tt.Texts())
	}

	for later.step() { // b is read, and a's preview rendered
	}
	tt.Frame()
	if f.file != "" || f.result.file != a || len(f.history) != 2 || f.history[0].file != b || f.phase != done {
		t.Errorf("reading %q, showing %+v, history %d long", f.file, f.result, len(f.history))
	}
	if attended != 1 || f.dock[len(f.dock)-1] != -1 {
		t.Errorf("attended %d times, Dock %v", attended, f.dock)
	}
}

// TestStartNew goes from a document to the welcome with New, keeping the
// history, whose row shows the document again. A PDF dropped there while
// another is read waits, and the progress shows.
func TestStartNew(t *testing.T) {
	f := newFixture(t, readFake(t))
	later := &spawnLater{}
	f.fx.spawn = later.spawn
	a, b := filepath.Join(f.dir, "a.pdf"), filepath.Join(f.dir, "b.pdf")
	tt := ui.NewTester(f.view, 960, 720)
	f.add(a)
	for later.step() {
	}
	tt.Frame()
	if f.phase != done || !f.menus[len(f.menus)-1].startNew {
		t.Fatalf("phase %v, menu %+v", f.phase, f.menus[len(f.menus)-1])
	}

	click(t, tt, "New")
	wantTexts(t, tt, "Drop a scanned PDF here", "a.pdf")
	if f.phase != idle || f.result != nil || len(f.history) != 1 {
		t.Errorf("phase %v, showing %+v, history %d long", f.phase, f.result, len(f.history))
	}
	if _, ok := tt.Find("New"); ok || f.menus[len(f.menus)-1].startNew {
		t.Errorf("New can be chosen on the welcome: menu %+v", f.menus[len(f.menus)-1])
	}
	click(t, tt, "a.pdf")
	if f.phase != done || f.result != f.history[0] {
		t.Errorf("phase %v, showing %+v", f.phase, f.result)
	}

	f.add(b)
	tt.Frame()
	click(t, tt, "a.pdf") // the document again, as b is read
	click(t, tt, "New")
	f.drop([]string{f.scan})
	tt.Frame()
	if f.phase != working || f.file != b || !slices.Equal(f.queue, []string{f.scan}) {
		t.Errorf("phase %v, reading %q, queue %q", f.phase, f.file, f.queue)
	}
	wantTexts(t, tt, "1 of 2 PDFs")
}

// TestGoToPage goes to the page whose number is typed in place of the
// page shown's: Enter goes there, within the document, and leaves the
// arrows turning the page; Escape, or a click elsewhere, goes nowhere.
func TestGoToPage(t *testing.T) {
	pages := make([]pdfocr.Page, 5)
	for i := range pages {
		pages[i] = pdfocr.Page{Text: fmt.Sprint("Page ", i+1), Lines: 1}
	}
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		return &result{pages: pages, pdf: tempFile(t, "%PDF"), textPDF: tempFile(t, "%PDF text")}, nil
	})
	tt := ui.NewTester(f.view, 960, 720)
	click(t, tt, "Choose PDF…")
	if !f.menus[len(f.menus)-1].goTo {
		t.Errorf("Go to Page can't be chosen: menu %+v", f.menus[len(f.menus)-1])
	}
	jump := func(typed string, key ui.Key) {
		t.Helper()
		click(t, tt, "Go to Page")
		if _, ok := tt.Find("Page"); !ok || !f.jumping {
			t.Fatalf("no field for the page's number in %q", tt.Texts())
		}
		tt.Type(typed)
		tt.Frame()
		if key != 0 {
			tt.Key(0, key)
			tt.Frame()
		}
	}

	jump("4", ui.KeyEnter)
	wantTexts(t, tt, "4 of 5")
	if f.jumping || f.at != 3 {
		t.Errorf("jumping %v, at page %d", f.jumping, f.at+1)
	}
	tt.Key(0, ui.KeyRight)
	tt.Frame()
	if f.at != 4 {
		t.Errorf("Right after the jump shows page %d", f.at+1)
	}

	jump("99", ui.KeyEnter)
	if f.at != 4 {
		t.Errorf("page 99 of 5 shows page %d", f.at+1)
	}
	jump("2", 0)
	tt.Key(0, ui.KeyLeft) // in the field, the arrows move the caret
	tt.Frame()
	if f.at != 4 {
		t.Errorf("Left in the field shows page %d", f.at+1)
	}
	tt.Key(0, ui.KeyEscape)
	tt.Frame()
	if f.jumping || f.at != 4 {
		t.Errorf("after Escape, jumping %v, at page %d", f.jumping, f.at+1)
	}

	jump("1", 0)
	click(t, tt, "Retyped PDF")
	if f.jumping || f.at != 4 {
		t.Errorf("after a click elsewhere, jumping %v, at page %d", f.jumping, f.at+1)
	}
}

// TestQueueFailure goes on with the queue past a PDF that cannot be read,
// saying so in a toast.
func TestQueueFailure(t *testing.T) {
	read := readFake(t)
	f := newFixture(t, func(ctx context.Context, path string, p pdfocr.Progress) (*result, error) {
		if filepath.Base(path) == "b.pdf" {
			return nil, errors.New("damaged")
		}
		return read(ctx, path, p)
	})
	later := &spawnLater{}
	f.fx.spawn = later.spawn
	tt := ui.NewTester(f.view, 960, 720)
	f.drop([]string{filepath.Join(f.dir, "a.pdf"), filepath.Join(f.dir, "b.pdf"), filepath.Join(f.dir, "c.pdf")})
	later.step() // a
	later.step() // b fails
	tt.Frame()
	wantTexts(t, tt, "Couldn’t read “b.pdf”")
	for later.step() {
	}
	tt.Frame()
	if len(f.history) != 2 || f.history[0].file != filepath.Join(f.dir, "c.pdf") || f.err != "" || f.file != "" {
		t.Errorf("history %+v, error %q, reading %q", f.history, f.err, f.file)
	}
}

// TestCancelAll cancels the PDF being read, and the next one's progress
// shows; Cancel All cancels it and those waiting.
func TestCancelAll(t *testing.T) {
	f := newFixture(t, readFake(t))
	later := &spawnLater{}
	f.fx.spawn = later.spawn
	tt := ui.NewTester(f.view, 960, 720)
	f.drop([]string{filepath.Join(f.dir, "a.pdf"), filepath.Join(f.dir, "b.pdf"), filepath.Join(f.dir, "c.pdf")})
	tt.Frame()
	click(t, tt, "Cancel")
	if f.phase != working || f.file != filepath.Join(f.dir, "b.pdf") || len(f.queue) != 1 {
		t.Fatalf("phase %v, reading %q, queue %q", f.phase, f.file, f.queue)
	}
	wantTexts(t, tt, "2 of 3 PDFs")
	click(t, tt, "Cancel All")
	if f.phase != idle || f.file != "" || len(f.queue) != 0 {
		t.Errorf("phase %v, reading %q, queue %q", f.phase, f.file, f.queue)
	}
	for later.step() { // the readings canceled end
	}
	tt.Frame()
	if len(f.history) != 0 || f.phase != idle || f.dock[len(f.dock)-1] != -1 {
		t.Errorf("history %+v, phase %v, Dock %v", f.history, f.phase, f.dock)
	}
}

// TestBrowseWhileReading shows a document chosen in the history while a
// PDF is read, its previews rendered meanwhile; the row of the PDF being
// read shows its progress again.
func TestBrowseWhileReading(t *testing.T) {
	f := newFixture(t, readFake(t))
	old := &result{file: filepath.Join(f.dir, "old.pdf"), read: time.Now(), pages: []pdfocr.Page{{Text: "Old", Lines: 1}}, pdf: tempFile(t, "%PDF old")}
	f.history = []*result{old}
	later := &spawnLater{}
	f.fx.spawn = later.spawn
	tt := ui.NewTester(f.view, 960, 720)
	f.add(f.scan)
	tt.Frame()
	wantTexts(t, tt, "Cancel", "old.pdf")

	click(t, tt, "old.pdf")
	if f.phase != done || f.result != old || f.file != f.scan {
		t.Fatalf("phase %v, showing %+v, reading %q", f.phase, f.result, f.file)
	}
	reading := later.fns[0]
	later.fns = later.fns[1:]
	for later.step() { // old's preview, while scan.pdf is read
	}
	if !slices.Contains(f.rendered, old.pdf+"#0") {
		t.Errorf("rendered %q while reading", f.rendered)
	}

	click(t, tt, "Reading scan.pdf")
	if f.phase != working {
		t.Errorf("phase %v after choosing the PDF being read", f.phase)
	}
	reading()
	tt.Frame()
	if f.phase != done || f.result.file != f.scan || len(f.history) != 2 {
		t.Errorf("phase %v, showing %+v", f.phase, f.result)
	}
}
