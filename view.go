package main

import (
	"fmt"
	"math"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"unscan/internal/pdfocr"
)

const (
	barHeight    = 52 // the title bar the view draws, under the window controls
	footerHeight = 40
	pageMargin   = 24 // around a page's preview
)

// Icons, drawn in strokes of the text's color.
var (
	documentIcon = icon(`<path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/><path d="M9 13h6M9 17h4"/>`)
	previousIcon = icon(`<path d="m15 18-6-6 6-6"/>`)
	nextIcon     = icon(`<path d="m9 18 6-6-6-6"/>`)
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
		switch a.phase {
		case idle:
			a.welcome(c, dragging)
		case working:
			a.progress(c)
		case done:
			a.document(c, dragging)
		}
		a.fontsDialog(c)
	})
}

// titleBar is the bar under the window controls: the name of the PDF and,
// once it is read, the form it shows in and what to do with it.
func (a *app) titleBar(c *ui.Context) {
	t := c.Theme()
	bar := c.TitleBar()
	row := ui.Row(c).Height(barHeight).Shrink(0).Padding(0, bar.Right+12, 0, bar.Left+16).Gap(12).DragWindow()
	if a.phase == idle {
		return
	}
	row.BorderWidth(0, 0, 1, 0).BorderColor(t.Border)
	row.Children(func() {
		name := a.file
		if a.phase == done {
			name = a.result.file
		}
		title := ui.Row(c).Grow(1).Basis(0).MinWidth(0)
		title.Children(func() { ui.Text(c, filepath.Base(name)).Bold().SingleLine() })
		if a.phase != done {
			return
		}
		a.formPicker(c)
		ui.Row(c).Grow(1).Basis(0).Justify(ui.End).Gap(8).Children(func() {
			if a.form == plainText && ui.Button(c, "Copy All").Clicked() {
				c.WriteClipboard(pdfocr.Text(a.result.pages))
				c.Toast("Copied the text of every page")
			}
			ui.PrimaryButton(c, "Save…").OnClick(a.save)
		})
	})
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
	value, status, left := -1.0, "Opening…", ""
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
	ui.Column(c).Grow(1).Center().Padding(24).Children(func() {
		ui.Column(c).Width(360).MaxWidthPercent(100).Gap(8).Children(func() {
			ui.Progress(c, value).Label("Reading " + filepath.Base(a.file))
			ui.Row(c).Gap(12).FontFeatures("tnum").Children(func() {
				ui.Text(c, status).Grow(1)
				ui.Text(c, left).TextColor(t.TextMuted)
			})
			ui.Button(c, "Cancel").AlignSelf(ui.Center).Margin(16, 0, 0, 0).OnClick(a.stop)
		})
	})
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
	body := ui.Box(c).Grow(1).MinHeight(0).Background(t.Surface).Focusable().FocusRing(false).Bind(&a.pageArea)
	body.Children(func() {
		if a.form == plainText {
			a.pageText(c, page)
		} else {
			a.pageImage(c)
		}
		if dragging {
			ui.Column(c).Absolute().Top(8).Right(8).Bottom(8).Left(8).Center().Radius(12).
				Border(2, t.Accent).BorderStyle(ui.BorderDashed).Background(t.Background.Alpha(0.9)).
				Children(func() { ui.Text(c, "Drop to read this PDF instead").FontSize(17).Bold() })
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
		if len(r.pages) < 2 {
			return
		}
		iconButton(c, previousIcon, "Previous Page", "←", a.at == 0).OnClick(func() { a.turn(a.at - 1) })
		ui.Textf(c, "%d of %d", a.at+1, len(r.pages)).FontFeatures("tnum").MinWidth(64).TextAlign(ui.Center)
		iconButton(c, nextIcon, "Next Page", "→", a.at == len(r.pages)-1).OnClick(func() { a.turn(a.at + 1) })
	})
}

// iconButton is a button showing an icon, with no face until hovered, as
// in a toolbar. A toolbar's arrows would move along it, where these turn
// the page.
func iconButton(c *ui.Context, icon *ui.SVG, label, key string, disabled bool) ui.Element {
	t := c.Theme()
	b := ui.ButtonBase(c).Label(label).Tooltip(label + " (" + key + ")").Disabled(disabled).
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

// pageImage shows the page as large as fits, as on paper.
func (a *app) pageImage(c *ui.Context) {
	t := c.Theme()
	p, ok := a.previews[preview{a.form, a.at}]
	ui.Column(c).Fill().Center().Padding(pageMargin).Children(func() {
		switch {
		case !ok:
			ui.Spinner(c)
		case p.err != nil:
			ui.Text(c, "This page can’t be shown: "+p.err.Error()).TextColor(t.TextMuted).MaxWidth(440).TextAlign(ui.Center)
		default:
			w, h := c.Size()
			w, h = w-2*pageMargin, h-barHeight-footerHeight-2*pageMargin
			bw, bh := p.bitmap.Size()
			scale := min(w/float32(bw), h/float32(bh))
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
	ui.Scroll(c.Key(a.at)).Fill().Background(t.Background).Children(func() {
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

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
