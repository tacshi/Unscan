package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/egoist/mygo/ui"

	"unscan/internal/pdfocr"
)

// The history keeps the PDFs read before, newest first, until the user
// removes them: each in a folder of its own in the history folder, holding
// the PDFs made of it and resultFile, written last, so that a folder
// without it is a reading that did not end.
const resultFile = "result.json"

// sidebarWidth is the width of the sidebar listing the history.
const sidebarWidth = 240

// savedResult is what resultFile keeps of a result, besides its PDFs.
type savedResult struct {
	File  string        `json:"file"` // the PDF read, where it was
	Read  time.Time     `json:"read"`
	Pages []pdfocr.Page `json:"pages"`
}

// save writes what r keeps besides its PDFs to its folder, which makes it
// part of the history.
func (r *result) save() error {
	data, err := json.Marshal(savedResult{File: r.file, Read: r.read, Pages: r.pages})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dir, resultFile), data, 0o600)
}

// loadResult reads the result kept in the folder dir.
func loadResult(dir string) (*result, error) {
	data, err := os.ReadFile(filepath.Join(dir, resultFile)) // #nosec G304 -- the history's own
	if err != nil {
		return nil, err
	}
	var saved savedResult
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	if len(saved.Pages) == 0 {
		return nil, errors.New("no pages")
	}
	return &result{
		file: saved.File, read: saved.Read, pages: saved.Pages,
		pdf: filepath.Join(dir, "searchable.pdf"), textPDF: filepath.Join(dir, "text.pdf"),
		dir: dir, size: dirSize(dir),
	}, nil
}

// loadHistory reads the history kept in the folder dir, newest first. A
// folder that holds no result is a reading that did not end: it is left
// alone for a day, as another copy of the app may still be reading, and
// removed after.
func loadHistory(dir string, now time.Time) []*result {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var history []*result
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name())
		r, err := loadResult(path)
		if err != nil {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > 24*time.Hour {
				_ = os.RemoveAll(path) // tried again next time
			}
			continue
		}
		history = append(history, r)
	}
	slices.SortFunc(history, func(a, b *result) int { return b.read.Compare(a.read) })
	return history
}

// dirSize is the size of the files in dir, in bytes.
func dirSize(dir string) int64 {
	var size int64
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}

// listed reports whether the sidebar has PDFs to list: those read, and
// those waiting to be.
func (a *app) listed() bool { return len(a.history) > 0 || len(a.queue) > 0 }

// showsSidebar reports whether the sidebar listing the PDFs shows.
func (a *app) showsSidebar() bool { return a.sidebar && a.listed() }

// toggleSidebar shows the sidebar, or hides it.
func (a *app) toggleSidebar() { a.sidebar = !a.sidebar }

// askRemove asks whether to remove rs from the history, with their files.
func (a *app) askRemove(rs ...*result) {
	if len(rs) > 0 {
		a.removing, a.asking = slices.Clone(rs), true
	}
}

// removeFromHistory removes rs from the history, and their files. In
// place of one shown, the one below it shows, else the one above, else
// none; where the progress shows, it stays.
func (a *app) removeFromHistory(rs []*result) {
	gone := func(r *result) bool { return slices.Contains(rs, r) }
	if a.result != nil && gone(a.result) {
		i := slices.Index(a.history, a.result)
		var next *result
		for _, r := range a.history[i+1:] {
			if !gone(r) {
				next = r
				break
			}
		}
		for j := i - 1; next == nil && j >= 0; j-- {
			if !gone(a.history[j]) {
				next = a.history[j]
			}
		}
		if a.phase == working {
			a.setResult(next)
		} else {
			a.show(next)
		}
	}
	a.history = slices.DeleteFunc(a.history, gone)
	a.fx.spawn(func() {
		var errs []error
		for _, r := range rs {
			if r.dir != "" {
				errs = append(errs, os.RemoveAll(r.dir))
			}
		}
		if err := errors.Join(errs...); err != nil {
			a.fx.update(func() {
				a.toast = &toast{title: "Couldn’t remove all the files", detail: err.Error()}
			})
		}
	})
}

// historySidebar lists the PDF being read and those waiting, then those
// read before, newest first, of which the one chosen shows. Its rows'
// menus remove them, as Delete does.
func (a *app) historySidebar(c *ui.Context) {
	t := c.Theme()
	now := time.Now()
	a.chosen = -1
	if a.phase == done {
		a.chosen = slices.Index(a.history, a.result)
	}
	l := &a.historyList
	l.Key = func(i int) any { return a.history[i] }
	l.Label = func(i int) string { return filepath.Base(a.history[i].file) }
	l.Selected = &a.chosen
	removeChosen := func() {
		if a.chosen >= 0 && a.chosen < len(a.history) {
			a.askRemove(a.history[a.chosen])
		}
	}
	l.OnShortcut(c, 0, ui.KeyBackspace, removeChosen)
	l.OnShortcut(c, 0, ui.KeyDelete, removeChosen)
	side := ui.Column(c).Width(sidebarWidth).Shrink(0).Background(t.Surface).BorderWidth(0, 1, 0, 0).BorderColor(t.Border)
	side.Children(func() {
		if a.file != "" {
			// At most half the sidebar, scrolling, so that the history shows.
			ui.Scroll(c.Key("queue")).Shrink(0).MaxHeightPercent(50).BorderWidth(0, 0, 1, 0).BorderColor(t.Border).Children(func() {
				ui.Column(c).Padding(t.Space(1)).Gap(2).Children(func() {
					a.readingRow(c)
					for _, path := range a.queue {
						a.queuedRow(c.Key(path), path)
					}
				})
			})
		}
		list := ui.List(c.Key("history"), l, len(a.history)).Grow(1).MinHeight(0).Padding(t.Space(1)).Label("History").
			OnChange(func() {
				if a.chosen >= 0 && a.chosen < len(a.history) {
					a.show(a.history[a.chosen])
				}
			})
		list.Rows(func(row ui.ListRow) {
			c, r := row.Context, a.history[row.Index]
			muted := t.TextMuted
			if row.Selected() {
				muted = t.AccentText.Alpha(0.8)
			}
			item := ui.Column(c).Padding(t.Space(1.5), t.Space(2)).Gap(2)
			item.ContextMenu(func(m *ui.Menu) {
				if m.Item(revealScanLabel()).Disabled(!exists(r.file)).Chosen() {
					a.fx.reveal(r.file)
				}
				m.Separator()
				if m.Item("Remove from History…").Chosen() {
					a.askRemove(r)
				}
			})
			item.Children(func() {
				ui.Text(c, filepath.Base(r.file)).Bold().SingleLine()
				ui.Text(c, plural(len(r.pages), "page")+" · "+when(r.read, now)).FontSize(12).TextColor(muted).SingleLine()
			})
		})
		ui.Row(c).Height(footerHeight).Shrink(0).Padding(0, 8, 0, 12).Gap(8).BorderWidth(1, 0, 0, 0).BorderColor(t.Border).
			Children(func() {
				ui.Text(c, a.historySize()).FontSize(12).TextColor(t.TextMuted).FontFeatures("tnum").SingleLine().Grow(1).MinWidth(0)
				ui.Button(c.Key("clear"), "Clear…").Tooltip("Clear History").OnClick(func() { a.askRemove(a.history...) })
			})
	})
}

// readingRow shows the PDF being read and how far it has come: chosen, its
// progress shows.
func (a *app) readingRow(c *ui.Context) {
	t := c.Theme()
	value, status, _ := a.status()
	row := ui.ButtonBase(c.Key("reading")).Label("Reading "+filepath.Base(a.file)).
		Padding(t.Space(1.5), t.Space(2)).Radius(t.Radius).OnClick(a.watch)
	switch {
	case a.phase == working:
		row.Background(t.Accent.Alpha(0.14))
	case row.Pressed():
		row.Background(t.SurfacePressed)
	case row.Hovered():
		row.Background(t.SurfaceHover)
	}
	row.Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(4).Children(func() {
			ui.Text(c, filepath.Base(a.file)).Bold().SingleLine()
			ui.Progress(c, value)
			ui.Text(c, status).FontSize(12).TextColor(t.TextMuted).FontFeatures("tnum").SingleLine()
		})
	})
}

// queuedRow shows a PDF waiting to be read, with a button that takes it
// out of the queue.
func (a *app) queuedRow(c *ui.Context, path string) {
	t := c.Theme()
	ui.Row(c).Padding(t.Space(1), t.Space(1), t.Space(1), t.Space(2)).Gap(4).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Gap(2).Children(func() {
			ui.Text(c, filepath.Base(path)).SingleLine()
			ui.Text(c, "Waiting").FontSize(12).TextColor(t.TextMuted).SingleLine()
		})
		iconButton(c, removeIcon, "Remove from Queue", "", false).OnClick(func() { a.unqueue(path) })
	})
}

// historySize says how many PDFs the history keeps, and in how much room.
func (a *app) historySize() string {
	var size int64
	for _, r := range a.history {
		size += r.size
	}
	return plural(len(a.history), "document") + " · " + byteSize(size)
}

// historyAlert asks whether to remove the PDFs chosen from the history.
func (a *app) historyAlert(c *ui.Context) {
	var title, msg, button string
	switch n := len(a.removing); {
	case n == 1:
		title = fmt.Sprintf("Remove “%s” from history?", filepath.Base(a.removing[0].file))
		msg = "Its searchable and retyped PDFs will be deleted. The scan itself isn’t touched."
		button = "Remove"
	case n > 1:
		var size int64
		for _, r := range a.removing {
			size += r.size
		}
		title = "Clear history?"
		msg = fmt.Sprintf("The searchable and retyped PDFs of %s (%s) will be deleted. The scans themselves aren’t touched.",
			plural(n, "document"), byteSize(size))
		button = "Clear History"
	}
	switch ui.AlertDialog(c, &a.asking, title, msg, "Cancel", button) {
	case 1:
		a.removeFromHistory(a.removing)
		a.removing = nil
	case 0, ui.AlertDismissed:
		a.removing = nil
	}
}

// when says when a PDF was read, as of now.
func when(t, now time.Time) string {
	t, now = t.Local(), now.Local()
	day := func(t time.Time) string { return t.Format(time.DateOnly) }
	switch day(t) {
	case day(now):
		return "Today, " + t.Format("15:04")
	case day(now.AddDate(0, 0, -1)):
		return "Yesterday, " + t.Format("15:04")
	}
	switch {
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2, 2006")
	}
}

// byteSize says how big n bytes are, in decimal units, as Finder does.
func byteSize(n int64) string {
	const units = "KMGT"
	if n < 1000 {
		return plural(int(n), "byte")
	}
	v, i := float64(n)/1000, 0
	for v >= 999.5 && i < len(units)-1 {
		v, i = v/1000, i+1
	}
	if v < 9.95 {
		return fmt.Sprintf("%.1f %cB", v, units[i])
	}
	return fmt.Sprintf("%.0f %cB", v, units[i])
}

// exists reports whether there is a file at path.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
