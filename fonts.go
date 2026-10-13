package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"unicode"
	"unicode/utf8"

	"github.com/egoist/mygo/ui"

	"unscan/internal/pdfocr"
)

// fontFaces are the faces retyped PDFs are set in, in the order of
// pdfocr.TextFonts, which the user may choose font files of their own for.
// Key names a face in the fonts saved, as cmd/unscan's flags do.
var fontFaces = [...]struct {
	key, name, builtIn string
	bold               bool
}{
	{"serif", "Serif", "Noto Serif SC", false},
	{"serif-bold", "Serif bold", "Noto Serif SC Bold", true},
	{"sans", "Sans-serif", "Noto Sans SC", false},
	{"sans-bold", "Sans-serif bold", "Noto Sans SC Bold", true},
	{"kai", "Kai (楷体)", "LXGW WenKai", false},
}

// fontChoice is a font file the user chose for a face, and which of the
// fonts in it: -1 for the one its weight picks (see pdfocr.PickFont),
// until it is read.
type fontChoice struct {
	Path  string   `json:"path"`
	Index int      `json:"index"`
	names []string // the fonts in the file, once read
	name  string   // the one chosen, which the dialog's menu shows
	err   string   // why it cannot be used, once read
}

// fontChoices are the user's fonts by face, of which a face's zero value
// sets it in its built-in font.
type fontChoices [len(fontFaces)]fontChoice

// chooseFont asks for a font file to set face i in.
func (a *app) chooseFont(i int) {
	a.fx.spawn(func() {
		path, err := a.fx.chooseFont()
		a.fx.update(func() {
			switch {
			case err != nil:
				a.toast = &toast{title: "Couldn’t choose a font", detail: err.Error()}
			case path != "":
				a.fonts[i] = fontChoice{Path: path, Index: -1}
				a.fontsChanged()
			}
		})
	})
}

// pickFont sets face i in the font of its file that the dialog's menu
// shows.
func (a *app) pickFont(i int) {
	f := &a.fonts[i]
	if j := slices.Index(f.names, f.name); j >= 0 && j != f.Index {
		f.Index = j
		a.fontsChanged()
	}
}

// resetFont sets face i in its built-in font again.
func (a *app) resetFont(i int) {
	a.fonts[i] = fontChoice{}
	a.fontsChanged()
}

// fontsChanged saves the fonts chosen, and sets retyped PDFs in them.
func (a *app) fontsChanged() {
	if err := a.fx.saveFonts(a.fonts); err != nil {
		a.toast = &toast{title: "Couldn’t save the fonts chosen", detail: err.Error()}
	}
	a.useFonts()
}

// useFonts sets retyped PDFs in the fonts chosen, in the background, once
// the models have loaded, and learns the fonts in each file and why one
// cannot be used. Fonts chosen meanwhile are used once it is done.
func (a *app) useFonts() {
	switch {
	case a.fx.useFonts == nil:
		return // loaded uses them
	case a.usingFonts:
		a.fontsAgain = true
		return
	}
	a.usingFonts = true
	fonts, use := a.fonts, a.fx.useFonts
	a.fx.spawn(func() {
		used := use(fonts)
		a.fx.update(func() {
			a.usingFonts = false
			if a.fontsAgain {
				a.fontsAgain = false
				a.useFonts()
				return
			}
			a.fonts = used
		})
	})
}

// useFonts reads the font files chosen, and sets conv's retyped PDFs in
// those that can be read. It returns the choices with the fonts in each
// file, the one chosen, and why one cannot be used.
func useFonts(conv *pdfocr.Converter, fonts fontChoices) fontChoices {
	var use [len(fontFaces)]pdfocr.Font
	for i := range fonts {
		f := &fonts[i]
		if f.Path == "" {
			continue
		}
		font, names, err := pdfocr.ReadFont(f.Path, f.Index, fontFaces[i].bold)
		f.names, f.name, f.err = names, "", ""
		if names != nil {
			f.Index, f.name = font.Index, names[font.Index]
		}
		if err != nil {
			f.err = fontError(err)
			continue
		}
		use[i] = font
	}
	err := conv.SetUserFonts(pdfocr.TextFonts{Serif: use[0], SerifBold: use[1], Sans: use[2], SansBold: use[3], Kai: use[4]})
	for i := range fonts {
		if err != nil && use[i].Data != nil {
			fonts[i].err = fontError(err)
		}
	}
	return fonts
}

// fontError says why a font file cannot be used, as a sentence.
func fontError(err error) string {
	if errors.Is(err, fs.ErrNotExist) {
		return "The file isn’t there anymore."
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	msg := err.Error()
	r, n := utf8.DecodeRuneInString(msg)
	return string(unicode.ToUpper(r)) + msg[n:] + "."
}

// loadFonts reads the fonts chosen from the file at path; a file missing
// or damaged chooses none.
func loadFonts(path string) fontChoices {
	var fonts fontChoices
	data, err := os.ReadFile(path) // #nosec G304 -- the app's own settings
	if err != nil {
		return fonts
	}
	var saved map[string]fontChoice
	if json.Unmarshal(data, &saved) != nil {
		return fonts
	}
	for i, face := range fontFaces {
		if f := saved[face.key]; f.Path != "" {
			fonts[i] = fontChoice{Path: f.Path, Index: max(f.Index, -1)}
		}
	}
	return fonts
}

// saveFonts writes the fonts chosen to the file at path.
func saveFonts(path string, fonts fontChoices) error {
	saved := map[string]fontChoice{}
	for i, f := range fonts {
		if f.Path != "" {
			saved[fontFaces[i].key] = f
		}
	}
	data, err := json.MarshalIndent(saved, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

// fontField shows the font face i is set in: its built-in one, the font
// file chosen, or a menu of the fonts in a collection, with buttons to
// choose another file and to go back to the built-in font.
func (a *app) fontField(c *ui.Context, i int) {
	t := c.Theme()
	face, f := fontFaces[i], &a.fonts[i]
	field := ui.Field(c.Key(face.key), face.name, func() {
		ui.Row(c).Gap(8).Children(func() {
			switch {
			case len(f.names) > 1:
				ui.Select(c.Key("font"), &f.name, f.names).Grow(1).MinWidth(0).OnChange(func() { a.pickFont(i) })
			case f.Path != "":
				ui.Text(c, cmp.Or(f.name, filepath.Base(f.Path))).Grow(1).MinWidth(0).SingleLine()
			default:
				ui.Text(c, face.builtIn).TextColor(t.TextMuted).Grow(1).MinWidth(0).SingleLine()
			}
			ui.Button(c.Key("choose"), "Choose…").OnClick(func() { a.chooseFont(i) })
			ui.Button(c.Key("reset"), "Reset").Tooltip("Use " + face.builtIn + " again").Disabled(f.Path == "").
				OnClick(func() { a.resetFont(i) })
		})
	})
	switch {
	case f.err != "":
		field.Error(f.err)
	case f.Path != "":
		field.Description(filepath.Base(f.Path))
	default:
		field.Description("Built in")
	}
}
