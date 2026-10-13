// Package ttf reads TrueType fonts (glyf outlines) and subsets them for
// embedding in PDFs: just enough of the format to set lines of text and to
// keep the glyphs a document uses.
package ttf

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"unicode/utf16"
)

// Font is a parsed TrueType font. Its methods are safe to call from several
// goroutines.
type Font struct {
	tables     map[string][]byte
	numGlyphs  int
	loca       []uint32 // numGlyphs+1 offsets into glyf
	advances   []uint16
	cmap       []cmapGroup // sorted by first
	UnitsPerEm int
	// Metrics in font units, as PDF font descriptors want them.
	Ascent, Descent, CapHeight int
	BBox                       [4]int // xMin, yMin, xMax, yMax
	PostScriptName             string
}

// cmapGroup maps the runes first through last to consecutive glyphs from
// glyph on.
type cmapGroup struct {
	first, last rune
	glyph       uint16
}

var errFormat = errors.New("ttf: malformed font")

// Parse reads a TrueType font. Collections and CFF outlines are not
// supported.
func Parse(data []byte) (*Font, error) {
	if len(data) < 12 {
		return nil, errFormat
	}
	switch v := binary.BigEndian.Uint32(data); v {
	case 0x00010000, 0x74727565: // 1.0, 'true'
	default:
		return nil, fmt.Errorf("ttf: not a TrueType font (version %#x)", v)
	}
	n := int(u16(data, 4))
	if len(data) < 12+16*n {
		return nil, errFormat
	}
	f := &Font{tables: map[string][]byte{}}
	for i := range n {
		r := data[12+16*i:]
		off, length := int(u32(r, 8)), int(u32(r, 12))
		if off < 0 || length < 0 || off+length > len(data) {
			return nil, fmt.Errorf("ttf: table %q is out of bounds", r[:4])
		}
		f.tables[string(r[:4])] = data[off : off+length]
	}
	for _, tag := range []string{"head", "hhea", "hmtx", "maxp", "loca", "glyf", "cmap"} {
		if f.tables[tag] == nil {
			return nil, fmt.Errorf("ttf: no %s table", tag)
		}
	}

	head := f.tables["head"]
	if len(head) < 54 {
		return nil, errFormat
	}
	f.UnitsPerEm = int(u16(head, 18))
	if f.UnitsPerEm == 0 {
		return nil, errFormat
	}
	f.BBox = [4]int{int(i16(head, 36)), int(i16(head, 38)), int(i16(head, 40)), int(i16(head, 42))}
	longLoca := i16(head, 50) == 1

	maxp := f.tables["maxp"]
	if len(maxp) < 6 {
		return nil, errFormat
	}
	f.numGlyphs = int(u16(maxp, 4))

	loca := f.tables["loca"]
	f.loca = make([]uint32, f.numGlyphs+1)
	glyfLen := uint32(len(f.tables["glyf"]))
	for i := range f.loca {
		if longLoca {
			if len(loca) < 4*(i+1) {
				return nil, errFormat
			}
			f.loca[i] = u32(loca, 4*i)
		} else {
			if len(loca) < 2*(i+1) {
				return nil, errFormat
			}
			f.loca[i] = 2 * uint32(u16(loca, 2*i))
		}
		if f.loca[i] > glyfLen || i > 0 && f.loca[i] < f.loca[i-1] {
			return nil, errFormat
		}
	}

	hhea := f.tables["hhea"]
	if len(hhea) < 36 {
		return nil, errFormat
	}
	f.Ascent, f.Descent = int(i16(hhea, 4)), int(i16(hhea, 6))
	metrics := int(u16(hhea, 34))
	hmtx := f.tables["hmtx"]
	if metrics == 0 || metrics > f.numGlyphs || len(hmtx) < 4*metrics {
		return nil, errFormat
	}
	f.advances = make([]uint16, f.numGlyphs)
	for g := range f.advances {
		f.advances[g] = u16(hmtx, 4*min(g, metrics-1))
	}

	if os2 := f.tables["OS/2"]; len(os2) >= 72 {
		f.Ascent, f.Descent = int(i16(os2, 68)), int(i16(os2, 70))
		if u16(os2, 0) >= 2 && len(os2) >= 90 {
			f.CapHeight = int(i16(os2, 88))
		}
	}
	if f.CapHeight == 0 {
		f.CapHeight = f.Ascent
	}

	var err error
	if f.cmap, err = parseCmap(f.tables["cmap"]); err != nil {
		return nil, err
	}
	f.PostScriptName = postScriptName(f.tables["name"])
	return f, nil
}

// NumGlyphs is how many glyphs the font has.
func (f *Font) NumGlyphs() int { return f.numGlyphs }

// Glyph is r's glyph, or 0 (.notdef) when the font has none.
func (f *Font) Glyph(r rune) uint16 {
	i, found := slices.BinarySearchFunc(f.cmap, r, func(g cmapGroup, r rune) int {
		switch {
		case g.last < r:
			return -1
		case g.first > r:
			return 1
		}
		return 0
	})
	if !found {
		return 0
	}
	g := f.cmap[i]
	gid := int(g.glyph) + int(r-g.first)
	if gid >= f.numGlyphs {
		return 0
	}
	return uint16(gid)
}

// Advance is the glyph's advance width in font units.
func (f *Font) Advance(g uint16) int {
	if int(g) >= f.numGlyphs {
		return 0
	}
	return int(f.advances[g])
}

// Bounds is the glyph's bounding box in font units: xMin, yMin, xMax and
// yMax. It is false for glyphs that draw nothing, such as spaces.
func (f *Font) Bounds(g uint16) ([4]int, bool) {
	data := f.glyph(g)
	if len(data) < 10 {
		return [4]int{}, false
	}
	return [4]int{int(i16(data, 2)), int(i16(data, 4)), int(i16(data, 6)), int(i16(data, 8))}, true
}

func (f *Font) glyph(g uint16) []byte {
	if int(g) >= f.numGlyphs {
		return nil
	}
	return f.tables["glyf"][f.loca[g]:f.loca[g+1]]
}

// components lists the glyphs a composite glyph is made of.
func (f *Font) components(g uint16) []uint16 {
	data := f.glyph(g)
	if len(data) < 10 || i16(data, 0) >= 0 {
		return nil
	}
	const (
		argsAreWords = 0x0001
		haveScale    = 0x0008
		more         = 0x0020
		haveXYScale  = 0x0040
		have2x2      = 0x0080
	)
	var out []uint16
	for p := 10; p+4 <= len(data); {
		flags := u16(data, p)
		out = append(out, u16(data, p+2))
		p += 4
		if flags&argsAreWords != 0 {
			p += 4
		} else {
			p += 2
		}
		switch {
		case flags&haveScale != 0:
			p += 2
		case flags&haveXYScale != 0:
			p += 4
		case flags&have2x2 != 0:
			p += 8
		}
		if flags&more == 0 {
			break
		}
	}
	return out
}

// parseCmap reads the font's Unicode mapping: a format 12 subtable when it
// has one, which covers every plane, or else a format 4 one.
func parseCmap(cmap []byte) ([]cmapGroup, error) {
	if len(cmap) < 4 {
		return nil, errFormat
	}
	best, bestRank := -1, 0
	for i := range int(u16(cmap, 2)) {
		r := 4 + 8*i
		if r+8 > len(cmap) {
			return nil, errFormat
		}
		platform, encoding, off := u16(cmap, r), u16(cmap, r+2), int(u32(cmap, r+4))
		if off+2 > len(cmap) {
			continue
		}
		format := u16(cmap, off)
		unicode := platform == 0 || platform == 3 && (encoding == 1 || encoding == 10)
		rank := 0
		switch {
		case unicode && format == 12:
			rank = 2
		case unicode && format == 4:
			rank = 1
		}
		if rank > bestRank {
			best, bestRank = off, rank
		}
	}
	if best < 0 {
		return nil, errors.New("ttf: no Unicode cmap")
	}
	t := cmap[best:]
	var groups []cmapGroup
	if bestRank == 2 {
		if len(t) < 16 {
			return nil, errFormat
		}
		n := int(u32(t, 12))
		if len(t) < 16+12*n {
			return nil, errFormat
		}
		for i := range n {
			g := t[16+12*i:]
			first, last, glyph := u32(g, 0), u32(g, 4), u32(g, 8)
			if first > last || last > 0x10FFFF || glyph > 0xFFFF {
				continue
			}
			groups = append(groups, cmapGroup{rune(first), rune(last), uint16(glyph)})
		}
	} else {
		if len(t) < 14 {
			return nil, errFormat
		}
		segs := int(u16(t, 6)) / 2
		ends, starts, deltas, ranges := 14, 16+2*segs, 16+4*segs, 16+6*segs
		if len(t) < 16+8*segs {
			return nil, errFormat
		}
		for s := range segs {
			start, end := rune(u16(t, starts+2*s)), rune(u16(t, ends+2*s))
			delta, rangeOff := u16(t, deltas+2*s), int(u16(t, ranges+2*s))
			for r := start; r <= end && r != 0xFFFF; r++ {
				var g uint16
				if rangeOff == 0 {
					g = uint16(r) + delta
				} else {
					p := ranges + 2*s + rangeOff + 2*int(r-start)
					if p+2 > len(t) {
						break
					}
					if g = u16(t, p); g != 0 {
						g += delta
					}
				}
				if g != 0 {
					groups = append(groups, cmapGroup{r, r, g})
				}
			}
		}
	}
	slices.SortFunc(groups, func(a, b cmapGroup) int { return int(a.first - b.first) })
	return groups, nil
}

// postScriptName is name ID 6, which PDFs use as the font's BaseFont.
func postScriptName(name []byte) string {
	if len(name) < 6 {
		return ""
	}
	count, strings := int(u16(name, 2)), int(u16(name, 4))
	for i := range count {
		r := 6 + 12*i
		if r+12 > len(name) {
			break
		}
		platform, id := u16(name, r), u16(name, r+6)
		length, off := int(u16(name, r+8)), strings+int(u16(name, r+10))
		if id != 6 || off+length > len(name) {
			continue
		}
		s := name[off : off+length]
		switch platform {
		case 1:
			return string(s)
		case 0, 3:
			units := make([]uint16, len(s)/2)
			for j := range units {
				units[j] = u16(s, 2*j)
			}
			return string(utf16.Decode(units))
		}
	}
	return ""
}

func u16(b []byte, off int) uint16 { return binary.BigEndian.Uint16(b[off:]) }
func i16(b []byte, off int) int16  { return int16(binary.BigEndian.Uint16(b[off:])) }
func u32(b []byte, off int) uint32 { return binary.BigEndian.Uint32(b[off:]) }
