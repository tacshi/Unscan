package main

import (
	"cmp"
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"unscan/internal/pdfocr"
)

const (
	barHeight     = 52 // the title bar the view draws, under the window controls
	footerHeight  = 40
	pageMargin    = 24 // around a page's preview
	captionHeight = 28 // of the name over each page compared
	turnSize      = 48 // of the buttons that turn the page, at its sides
	turnMargin    = 8  // around them
)

// Icons, drawn in strokes of the text's color.
var (
	newIcon      = icon(`<circle cx="12" cy="12" r="9"/><path d="M8 12h8M12 8v8"/>`)
	documentIcon = icon(`<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/><path d="M9 13h6M9 17h4"/>`)
	previousIcon = icon(`<path d="m15 18-6-6 6-6"/>`)
	nextIcon     = icon(`<path d="m9 18 6-6-6-6"/>`)
	removeIcon   = icon(`<path d="M18 6 6 18M6 6l12 12"/>`)
	sidebarIcon  = icon(`<rect width="18" height="18" x="3" y="3" rx="2"/><path d="M9 3v18"/>`)
	compareIcon  = icon(`<path d="M8 19H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2h3"/><path d="M16 5h3a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2h-3"/><path d="M12 4v16"/>`)
)

func icon(paths string) *ui.SVG {
	return ui.MustParseSVG([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="24" height="24" viewBox="0 0 24 24" ` +
		`fill="none" stroke="currentColor" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">` + paths + `</svg>`))
}

func (a *app) view(c *ui.Context) {
	a.syncMenu()
	a.renderPreviews()
	a.showToast(c)
	root := ui.Column(c).Fill()
	dragging := false
	if a.canOpen() {
		if files := root.DroppedFiles(); len(files) > 0 {
			a.drop(files)
		}
		dragging = root.FileDragOver()
	}
	root.Children(func() {
		a.titleBar(c)
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			if a.showsSidebar() {
				a.historySidebar(c)
			}
			ui.Column(c).Grow(1).MinWidth(0).Children(func() {
				switch a.phase {
				case idle:
					a.welcome(c, dragging)
				case working:
					a.progress(c)
				case done:
					a.document(c, dragging)
				}
			})
		})
		a.settingsDialog(c)
		a.historyAlert(c)
	})
}

// titleBar is the bar under the window controls: a button showing the
// history, the name of the PDF and, once it is read, the form it shows in
// and what to do with it.
func (a *app) titleBar(c *ui.Context) {
	t := c.Theme()
	bar := c.TitleBar()
	row := ui.Row(c).Height(barHeight).Shrink(0).Padding(0, bar.Right+12, 0, bar.Left+16).Gap(12).DragWindow()
	if a.phase == idle && !a.showsSidebar() {
		if a.listed() {
			row.Children(func() { a.sidebarButton(c) })
		}
		return
	}
	row.BorderWidth(0, 0, 1, 0).BorderColor(t.Border)
	row.Children(func() {
		title := ui.Row(c).Grow(1).Basis(0).MinWidth(0).Gap(8)
		title.Children(func() {
			if a.listed() {
				a.sidebarButton(c)
			}
			if a.phase == done {
				key := "⌘N"
				if runtime.GOOS != "darwin" {
					key = "Ctrl+N"
				}
				iconButton(c, newIcon, "New", key, false).OnClick(a.startNew)
			}
			switch a.phase {
			case working:
				ui.Text(c, filepath.Base(a.file)).Bold().SingleLine()
			case done:
				ui.Text(c, filepath.Base(a.result.file)).Bold().SingleLine()
			}
		})
		if a.phase != done {
			return
		}
		a.formPicker(c)
		ui.Row(c).Grow(1).Basis(0).Justify(ui.End).Gap(8).Children(func() {
			const compare = "Compare with Original"
			ui.Toggle(c, &a.comparing, "").Label(compare).Tooltip(compare).Disabled(a.form == searchable).
				Children(func() { ui.Icon(c, compareIcon) })
			if a.form == plainText && ui.Button(c, "Copy All").Clicked() {
				c.WriteClipboard(pdfocr.Text(a.result.pages))
				c.Toast("Copied the text of every page")
			}
			ui.PrimaryButton(c, "Save…").OnClick(a.save)
		})
	})
}

// sidebarButton shows the sidebar listing the history, or hides it.
func (a *app) sidebarButton(c *ui.Context) {
	key := "⌥⌘S"
	if runtime.GOOS != "darwin" {
		key = "Ctrl+Alt+S"
	}
	iconButton(c, sidebarIcon, sidebarLabel(a.sidebar), key, false).OnClick(a.toggleSidebar)
}

// sidebarLabel names showing the sidebar, or hiding it while it shows.
func sidebarLabel(shown bool) string {
	if shown {
		return "Hide Sidebar"
	}
	return "Show Sidebar"
}

// forms names the forms a document shows in.
var forms = [...]string{searchable: "Searchable PDF", retyped: "Retyped PDF", plainText: "Text"}

// formPicker chooses the form the document shows in, as the theme's
// segmented control does. A click on it leaves the keyboard focus on the
// page, whose arrows turn it; the arrows choose the form only when the
// focus came by the keyboard.
func (a *app) formPicker(c *ui.Context) {
	t := c.Theme()
	seg := ui.SegmentedBase(c, &a.form, len(forms))
	seg.Track.Label("Show as").AlignItems(ui.Stretch).Padding(t.Space(0.5)).Gap(t.Space(0.5)).
		Radius(t.Radius).Background(t.Surface).Border(1, t.Border)
	seg.Track.Children(func() {
		for i, name := range forms {
			s := seg.Segment(i).Padding(t.Space(1), t.Space(3)).Radius(max(t.Radius-t.Space(0.5), 0))
			switch {
			case i == a.form && t.Dark:
				s.Background(t.SurfacePressed).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.12))
			case i == a.form:
				s.Background(t.Background).Shadow(0, 1, 2, 0, ui.RGBA(0, 0, 0, 0.12))
			case s.Pressed():
				s.Background(t.SurfacePressed)
			case s.Hovered():
				s.Background(t.SurfaceHover)
			}
			if s.Clicked() && !s.FocusVisible() {
				a.pageArea.Focus(c)
			}
			s.Children(func() { ui.Text(c, name).SingleLine() })
		}
	})
}

// welcome asks for a PDF, which can be dropped anywhere in the window.
func (a *app) welcome(c *ui.Context, dragging bool) {
	t := c.Theme()
	zone := ui.Column(c).Grow(1).Margin(0, 16, 16, 16).Radius(12).Center().Gap(6).Padding(24)
	iconColor := t.TextMuted
	if dragging {
		zone.Border(2, t.Accent).BorderStyle(ui.BorderDashed).Background(t.Accent.Alpha(0.06))
		iconColor = t.Accent
	}
	zone.Children(func() {
		ui.Icon(c, documentIcon).FontSize(48).TextColor(iconColor).Margin(0, 0, 10, 0)
		if a.broken != "" {
			ui.Text(c, "Text recognition didn’t start").FontSize(20).Bold()
			ui.Text(c, a.broken).TextColor(t.TextMuted).MaxWidth(440).TextAlign(ui.Center).Selectable()
			return
		}
		ui.Text(c, "Drop a scanned PDF here").FontSize(20).Bold()
		ui.Text(c, "Unscan reads its text on this Mac, so you can search, select and copy it.").
			TextColor(t.TextMuted).MaxWidth(360).TextAlign(ui.Center)
		ui.PrimaryButton(c, "Choose PDF…").Margin(14, 0, 0, 0).OnClick(a.choose)
		if a.err != "" {
			ui.Text(c, a.err).TextColor(t.Danger).MaxWidth(440).TextAlign(ui.Center).Selectable().Margin(14, 0, 0, 0)
		}
	})
}

// progress shows how far reading has come.
func (a *app) progress(c *ui.Context) {
	t := c.Theme()
	value, status, left := a.status()
	ui.Column(c).Grow(1).Center().Padding(24).Children(func() {
		ui.Column(c).Width(360).MaxWidthPercent(100).Gap(8).Children(func() {
			ui.Progress(c, value).Label("Reading " + filepath.Base(a.file))
			ui.Row(c).Gap(12).FontFeatures("tnum").Children(func() {
				ui.Text(c, status).Grow(1)
				ui.Text(c, left).TextColor(t.TextMuted)
			})
			if at, of := a.batch(); of > 1 {
				ui.Textf(c, "%d of %d PDFs", at, of).TextColor(t.TextMuted).FontFeatures("tnum")
			}
			ui.Row(c).AlignSelf(ui.Center).Gap(8).Margin(16, 0, 0, 0).Children(func() {
				ui.Button(c, "Cancel").OnClick(a.stop)
				if len(a.queue) > 0 {
					ui.Button(c, "Cancel All").OnClick(a.stopAll)
				}
			})
		})
	})
}

// status says how far reading the PDF has come: the share of its pages
// read, -1 until they are counted, in words, and how long the rest take.
func (a *app) status() (value float64, status, left string) {
	value, status = -1, "Opening…"
	switch {
	case a.waiting:
		status = "Getting ready…"
	case a.pages > 0 && a.page == a.pages:
		status = "Finishing…"
	case a.pages > 0:
		value = float64(a.page) / float64(a.pages)
		status = fmt.Sprintf("%d of %s", a.page, plural(a.pages, "page"))
		left = timeLeft(time.Since(a.counted), a.page, a.pages)
	}
	return value, status, left
}

// timeLeft guesses how long the rest of the pages take, from how long done
// took, once enough have been read to tell.
func timeLeft(spent time.Duration, done, pages int) string {
	if done < 2 || spent < 5*time.Second {
		return ""
	}
	left := spent * time.Duration(pages-done) / time.Duration(done)
	switch minutes := int(math.Round(left.Minutes())); {
	case left < 45*time.Second:
		return "Less than a minute left"
	case minutes <= 1:
		return "About a minute left"
	default:
		return fmt.Sprintf("About %d minutes left", minutes)
	}
}

// document shows a page of the document read, in the form chosen, with a
// note on what that form is, and a way to the other pages.
func (a *app) document(c *ui.Context, dragging bool) {
	t := c.Theme()
	r := a.result
	if c.Shortcut(0, ui.KeyLeft) {
		a.turn(a.at - 1)
	}
	if c.Shortcut(0, ui.KeyRight) {
		a.turn(a.at + 1)
	}
	page := r.pages[a.at]
	// The room the page shows in.
	w, h := c.Size()
	if a.showsSidebar() {
		w -= sidebarWidth
	}
	h -= barHeight + footerHeight
	turns := len(r.pages) > 1
	if turns {
		w -= 2 * (turnSize + 2*turnMargin)
	}
	body := ui.Box(c).Grow(1).MinHeight(0).Background(t.Surface).Focusable().FocusRing(false).Bind(&a.pageArea)
	body.Children(func() {
		ui.Row(c).Grow(1).MinHeight(0).AlignItems(ui.Stretch).Children(func() {
			if turns {
				turnButton(c, previousIcon, "Previous Page", "←", a.at == 0).OnClick(func() { a.turn(a.at - 1) })
			}
			ui.Column(c).Grow(1).MinWidth(0).Children(func() {
				if a.compares() {
					a.compare(c, page, w, h)
				} else {
					a.pane(c, a.form, "", page, w, h)
				}
			})
			if turns {
				turnButton(c, nextIcon, "Next Page", "→", a.at == len(r.pages)-1).OnClick(func() { a.turn(a.at + 1) })
			}
		})
		if dragging {
			ui.Column(c).Absolute().Top(8).Right(8).Bottom(8).Left(8).Center().Radius(12).
				Border(2, t.Accent).BorderStyle(ui.BorderDashed).Background(t.Background.Alpha(0.9)).
				Children(func() { ui.Text(c, "Drop PDFs to read them").FontSize(17).Bold() })
		}
	})

	note := ""
	switch {
	case page.Skipped:
		note = "This page already had text, so it was left as it was."
	case a.form == searchable:
		note = "Looks just like the scan, with text you can search, select and copy."
	case a.form == retyped:
		note = "Retyped in real fonts on white paper. Misread characters show here."
	}
	ui.Row(c).Height(footerHeight).Shrink(0).Padding(0, 8, 0, 16).Gap(4).
		BorderWidth(1, 0, 0, 0).BorderColor(t.Border).Children(func() {
		ui.Text(c, note).FontSize(12).TextColor(t.TextMuted).SingleLine().Grow(1).MinWidth(0)
		if len(r.pages) > 1 {
			a.pageNumber(c, len(r.pages))
		}
	})
}

// pageNumber says which page shows, of how many. Clicked, or with Go to
// Page, it takes the number of a page to go to: Enter goes there, and
// Escape, or a click elsewhere, leaves it. Either way the focus goes back
// to the page, whose arrows turn it.
func (a *app) pageNumber(c *ui.Context, pages int) {
	t := c.Theme()
	if !a.jumping {
		key := "⌥⌘G"
		if runtime.GOOS != "darwin" {
			key = "Ctrl+Alt+G"
		}
		b := ui.ButtonBase(c).Label("Go to Page").Tooltip("Go to Page ("+key+")").
			Padding(t.Space(1), t.Space(2)).Radius(t.Radius).OnClick(a.goToPage)
		switch {
		case b.Pressed():
			b.Background(t.SurfacePressed)
		case b.Hovered():
			b.Background(t.SurfaceHover)
		}
		b.Children(func() { ui.Textf(c, "%d of %d", a.at+1, pages).FontFeatures("tnum") })
		return
	}
	ui.Row(c).Gap(t.Space(1.5)).Padding(0, t.Space(2)).Children(func() {
		in := ui.TextInput(c, &a.jumpTo).Width(t.Space(14)).Placeholder(strconv.Itoa(a.at + 1)).
			Label("Page").FontFeatures("tnum").AutoFocus()
		leave := func() {
			a.jumping = false
			a.pageArea.Focus(c)
		}
		switch {
		case in.Submitted():
			if n, err := strconv.Atoi(strings.TrimSpace(a.jumpTo)); err == nil {
				a.turn(n - 1)
			}
			leave()
		case in.Shortcut(0, ui.KeyEscape):
			leave()
		case !in.Focused():
			a.jumping = false // the focus went elsewhere, where it stays
		}
		ui.Textf(c, "of %d", pages).FontFeatures("tnum")
	})
}

// turnButton is a large round button that turns the page, in a margin of
// its own beside the room the page shows in, halfway down it: back on the
// left, on at the right, as the arrows turn it. At the first page or the
// last, it fades.
func turnButton(c *ui.Context, icon *ui.SVG, label, key string, disabled bool) ui.Element {
	t := c.Theme()
	side := ui.Column(c).Shrink(0).Padding(0, turnMargin).Justify(ui.Center)
	var b ui.Element
	side.Children(func() {
		b = ui.ButtonBase(c).Label(label).Tooltip(label+" ("+key+")").Disabled(disabled).
			Width(turnSize).Height(turnSize).Center().Radius(turnSize/2).
			Background(t.Background.Alpha(0.92)).Border(1, t.Border).Shadow(0, 2, 8, 0, ui.RGBA(0, 0, 0, 0.12))
		switch {
		case disabled:
			b.Opacity(0.35)
		case b.Pressed():
			b.Background(t.SurfacePressed)
		case b.Hovered():
			b.Background(t.SurfaceHover)
		}
		b.Children(func() { ui.Icon(c, icon).FontSize(24) })
	})
	return b
}

// iconButton is a button showing an icon, with no face until hovered, as
// in a toolbar. A toolbar's arrows would move along it, where these turn
// the page.
func iconButton(c *ui.Context, icon *ui.SVG, label, key string, disabled bool) ui.Element {
	t := c.Theme()
	tip := label
	if key != "" {
		tip += " (" + key + ")"
	}
	b := ui.ButtonBase(c).Label(label).Tooltip(tip).Disabled(disabled).
		Padding(t.Space(1.5)).Radius(t.Radius)
	switch {
	case disabled:
		b.TextColor(t.TextMuted.Alpha(0.5))
	case b.Pressed():
		b.Background(t.SurfacePressed)
	case b.Hovered():
		b.Background(t.SurfaceHover)
	}
	return b.Children(func() { ui.Icon(c, icon) })
}

// compare shows the original page beside the page in the form shown, in w
// by h, split where the user left it.
func (a *app) compare(c *ui.Context, page pdfocr.Page, w, h float32) {
	t := c.Theme()
	least := t.Space(10) // as narrow as the split lets a side be
	left := min(max(w*cmp.Or(a.split, 0.5), least), w-1-least)
	// In a box of their own, which the grip is placed in.
	ui.Box(c).Grow(1).MinHeight(0).Children(func() {
		ui.Split(c, &left, func() {
			a.pane(c, searchable, "Original", page, left, h)
		}, func() {
			a.pane(c, a.form, forms[a.form], page, w-1-left, h)
		}).Grow(1).MinHeight(0).OnChange(func() { a.split = left / w })

		// The split's divider takes the keyboard focus as its grip is
		// dragged, and the arrows then move it rather than turn the page:
		// this grip, over the split's, drags it with the focus left where
		// it was. The divider takes the focus from Tab alone, as the
		// arrows move it then.
		grip := t.Space(1.5) // as wide as the split's
		g := ui.Box(c).Absolute().Top(0).Bottom(0).Left(left - (grip-1)/2).Width(grip).Cursor(ui.CursorResizeEW)
		dx, _, held := g.Dragged()
		if held && dx != 0 {
			a.split = min(max(left+dx, least), w-1-least) / w
		}
		if held || g.Hovered() {
			g.Draw(func(p *ui.Painter, r ui.Rect) {
				p.Fill(ui.Rect{X: r.X + (grip-1)/2, Y: r.Y, W: 1, H: r.H}, t.Accent, 0)
			})
		}
	})
}

// pane shows the page in form, in w by h, under name when it is not "".
func (a *app) pane(c *ui.Context, form int, name string, page pdfocr.Page, w, h float32) {
	if name != "" {
		ui.Row(c).Height(captionHeight).Shrink(0).Justify(ui.Center).Children(func() {
			ui.Text(c, name).FontSize(12).TextColor(c.Theme().TextMuted).SingleLine()
		})
		h -= captionHeight
	}
	if form == plainText {
		a.pageText(c, page)
	} else {
		a.pageImage(c, form, w, h)
	}
}

// pageImage shows the page in form as large as fits in w by h, as on
// paper.
func (a *app) pageImage(c *ui.Context, form int, w, h float32) {
	t := c.Theme()
	p, ok := a.previews[preview{form, a.at}]
	ui.Column(c).Grow(1).MinHeight(0).Center().Padding(pageMargin).Children(func() {
		switch {
		case !ok:
			ui.Spinner(c)
		case p.err != nil:
			ui.Text(c, "This page can’t be shown: "+p.err.Error()).TextColor(t.TextMuted).MaxWidth(440).TextAlign(ui.Center)
		default:
			w, h := w-2*pageMargin, h-2*pageMargin
			bw, bh := p.bitmap.Size()
			scale := max(min(w/float32(bw), h/float32(bh)), 0)
			ui.Image(c, p.bitmap).Size(float32(bw)*scale, float32(bh)*scale).Label(fmt.Sprintf("Page %d", a.at+1)).
				Shadow(0, 1, 3, 0, ui.RGBA(0, 0, 0, 0.12)).Shadow(0, 6, 20, 0, ui.RGBA(0, 0, 0, 0.08))
		}
	})
}

// pageText shows the page's text in a column comfortable to read.
func (a *app) pageText(c *ui.Context, page pdfocr.Page) {
	t := c.Theme()
	text := strings.TrimSpace(page.Text)
	// Keyed by page, so that each opens at its top.
	ui.Scroll(c.Key(a.at)).Grow(1).MinHeight(0).Background(t.Background).Children(func() {
		col := ui.Column(c).MaxWidth(680).Margin(0, ui.Auto).Padding(32, 36).Selectable()
		col.Children(func() {
			if text == "" {
				ui.Text(c, "No text was found on this page.").TextColor(t.TextMuted)
				return
			}
			ui.Text(c, text).FontSize(15).LineHeight(1.6)
		})
	})
}

// showToast shows the message left by what the user did last.
func (a *app) showToast(c *ui.Context) {
	n := a.toast
	if n == nil {
		return
	}
	a.toast = nil
	if n.saved != "" {
		path, reveal := n.saved, a.fx.reveal
		c.ToastAction(n.title, revealLabel(), func() { reveal(path) })
		return
	}
	c.AddToast(ui.Toast{Title: n.title, Description: n.detail, Type: "error", Timeout: 8 * time.Second})
}

// revealLabel names showing a file in the file manager.
func revealLabel() string {
	if runtime.GOOS == "darwin" {
		return "Show in Finder"
	}
	return "Show in Folder"
}

// revealScanLabel names showing the PDF read in the file manager.
func revealScanLabel() string {
	if runtime.GOOS == "darwin" {
		return "Show Scan in Finder"
	}
	return "Show Scan in Folder"
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
