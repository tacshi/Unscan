# How Unscan works

This document covers what the [README](../README.md) leaves out: how
Unscan keeps memory flat on long documents, how it makes the retyped PDF,
and what its tests check.

The retyped PDF is called the *text PDF* in the code
(`internal/pdfocr/textpdf.go`, `redraw.go` and `style.go`) and on the
command line (`-text`), and so it is here.

## Long documents

Pages go through a pipeline: PDFium renders a page while the OCR models
read the one before, and pages read before that are redrawn for the text
PDF on two goroutines of their own, along with the part of judging each
line's face that needs no other line. The models take most of the time
and are kept busy. They run a thread per performance core, as threads on
the efficiency cores hold every step back, and the steps between their
runs (cutting out lines, scaling them, decoding what was read) run on
every core.

PDFium reads the scanned PDF from its file as it needs it, rather than
from a copy in memory, and the PDFs made are written to files as they
are saved; the app keeps them in its history folder, with the text of
each page, until the user removes them, and the command line version
writes them beside where they go and moves them there once whole. The
history needs no copy of the scan: the searchable PDF keeps its pages as
they were, under invisible text, so the app shows the original from it.
PDFium keeps what it reads of a document, the scan of every page it
renders, until the document is closed, so the pipeline opens it anew
every 32 pages. Its WebAssembly memory is mapped whole at the start, on
macOS and Linux, and filled in as it grows, rather than grown as a Go
slice, which is copied each time and leaves the copies to the garbage
collector. Only the pages along the pipeline are in memory at once, so
while pages are read, memory does not grow with the document: it is
mostly what the models need, about 3 GB.

Adding the text layer at the end has PDFium load every page once more,
keeping its scan until the document is closed, in memory that is 2 GB
at most. So the pages of an input bigger than 512 MB have their layer
added in batches, each saved to a temporary file that is opened anew for
the next, which lets go of the batch's scans; the saves take seconds.

A conversion holds its PDFium instance from start to end, so the pool
has a second, which renders the app's previews while a PDF is read, as
the user browses the ones read before. It is made from the module
compiled for the first, and goes once both are idle, with the memory it
grew into; rendering previews throughout a conversion added nothing
measurable to its peak memory.

## How the text PDF is made

For each scanned page, at the resolution it was read at:

1. The paper's color, the page's most common light color, is mapped to
   white, with the faint noise around it.
2. Each line's ink is measured inside its box, and past it where a tight
   box cuts glyphs: the box's edge gives the background, and the pixels
   that differ from it by half the ink's contrast are the ink, its edges
   read to a fraction of a pixel from how dark their pixels are. Where a
   box holds the edges of other lines too, the ink kept is what matches
   the height that the recognized text's glyphs give for its width. Dot
   leaders that a contents page's headings end with, or its page numbers
   start with, are kept as printed: the recognizer reads a few of the dots,
   which set in the heading's face would come out its size and weight.
3. Once every page is measured, each line's face is judged from its
   strokes, against each face's glyphs drawn at the line's size.
   Printing and scanning thicken every stroke by about as much, which
   blurs thick against thin; that thickening, which the document's body
   text shows over the regular faces, is taken off first. The kind is
   then that of the face whose upright strokes are as many times thicker
   than its level ones (a serif face's clearly are, a sans-serif face's
   are about as thick). Kai, whose brush gives its strokes every
   thickness, lies between the two by that measure, and is told by its
   edges instead: as its strokes slant and curve, more of them run aslant
   than the serif face's do, set in the same text, by more than any
   Song's or Hei's do, whatever the thickening. A paragraph is Kai by the
   median of its lines; a line alone, as a heading, must slant further,
   unless the lines of its size on its page are Kai, as a preface's first
   line is, and a line mostly of Latin, as an English title quoted in
   Chinese, follows them. FangSong, which slants as Kai does, is set in
   Kai too. A line is bold where its strokes are 15%
   thicker than those of its kind's lines on its page (or in the
   document, for a kind few of the page's lines are), as headings set in
   medium or bold faces are. Both are voted by paragraph (lines that
   follow each other down a page, as high as each other, in one script):
   a paragraph is set in the kind most of its lines lean to, and regular
   if most of them are, but for a CJK line leaning surely to the other
   kind, as a heading does, or a line surely heavier. A line too short to
   judge, as a paragraph's last of one character, takes its paragraph's.
4. The line is set in its face so that its glyphs' outlines cover its
   ink. Their height gives the font size, which lines within 7% of
   each other on a page share, as a paragraph's do. Glyphs keep their
   shapes: the line is spaced out to the ink's width as books are set,
   between CJK characters, three times as much where they meet Latin
   words, and at spaces, but not within words. Where gaps wider than the
   ink is high, by a quarter, part the ink, as a label from what it
   labels, the characters are matched to each stretch by how far their
   outlines span, parting where the text itself leaves a gap, as after a
   comma, and set on it,
   and ink no character is matched to, such as a bullet, is kept as
   printed. A line that has nowhere to space, or is wider than its ink,
   is stretched or squeezed (`Tz`) instead. The spacing goes into the
   characters' advances, with a CID for each character and advance, so
   characters abut and text extraction finds no spaces that were not
   read. A line in Kai takes its Latin letters, digits and narrow
   punctuation from the serif face, as books set them in Times beside
   Kai, since LXGW WenKai's own are a casual hand. Ink that is about
   black prints black.
5. Lines on plain paper are erased. What is left that is not white is cut
   into images: line art (rules, dot leaders) losslessly, the rest as JPEG
   at 200 DPI at most. Specks of dust are dropped, and so are the marks
   the scan's edges leave, the shadow of the paper's edge or the
   scanner's lid: what lies in the outer half of a margin, within 15 mm
   of the edge, as a line along it or a speck within 3 mm of it.

Each face is subset to the glyphs set in it, keeping their IDs, and only
faces a document uses are embedded; each character gets its own CID,
mapped back to it in one `ToUnicode` map the faces share, so text copies
out as it was read, whatever face it is set in. A character spaced out
in several ways takes a CID for each: a Chinese book takes some 45,000
in 600 pages, and more as it goes on, past the 65,534 that two bytes
number. So pages are numbered in sets, a page in one, each with subsets
of the faces and a map of its own; a shorter document takes one.

## What the tests check

```sh
CGO_ENABLED=0 go test ./...
```

- `internal/ocr`: the geometry, and OCR of rendered English and Chinese
  pages.
- `internal/ttf`: glyphs, advances and bounds agree with
  `golang.org/x/image/font/sfnt`, and a subset keeps the outlines of the
  glyphs used (composites with their parts) and drops the rest. The fonts
  of a collection read by their English names, and one subsets into a font
  of its own; fonts of PostScript outlines are refused, and fonts are
  embeddable as their `fsType` says.
- `internal/pdfocr` (text PDF): a line set in the font on yellowed paper
  comes back at its size and place, in a loose box or a tight one, with
  the picture beside it kept and the dust dropped; white text on a red
  band stays as printed under invisible text; a word set tighter than the
  font is squeezed to end where its ink does. The marks of a scan's
  edges (a broken line down one, a speck on another, a shadow along a
  third) are dropped, while a rule over the text, a folio the recognizer
  missed and a picture printed to the edge are kept. A line tracked as
  books are, and a label set apart from what it labels after a bullet, are
  redrawn unstretched, each character within 2–2.5 pixels of where it
  was drawn at 40 pixels an em, and extract from PDFium as they read.
  Lines measure within 1.5% of their size, from 24 to 64 pixels an em.
  Pages of lines in faces of other families (Songti SC, Hiragino Sans GB,
  Times New Roman, Arial, and Kaiti SC where macOS has downloaded it),
  blurred and noisy as scans are, are set in the text face of their kind
  and weight. A Kai paragraph's last line of one character, a preface's
  first line alone and a line mostly of English among Kai ones are Kai
  too, and a bold Song heading as slanted as Kai's least is not. A line
  set in Kai takes its Latin and digits from the serif face, and switches
  fonts where it does. Pages numbered in CID sets of their
  own extract and render pixel for pixel as numbered in one. End to end,
  the turned and offset pages of the scanned PDF become a text PDF whose
  pages keep their size, whose text extracts, whose type covers the
  scan's ink within half a millimeter at each edge, whose picture is
  kept, and whose paper is white; pages with text are kept as they were.
  Lines set in a font of the user's, Times of 2048 units an em from a
  collection, take the Chinese characters it lacks from the text font, as
  Kai's do from the text fonts' Kai face, are measured in the text fonts,
  embed both, extract as read, and render over the scan's ink from their
  first character to their last. Of Songti SC's collection, the regular
  and bold fonts are chosen by name, and its Black, whose license forbids
  embedding it, is refused.
- `internal/pdfocr`: a four-page scanned PDF (pages rotated 0°, 90°,
  180° and 270°, one with an offset MediaBox) is converted, then checked.
  The test confirms that PDFium extracts the text, that the text lies
  over the lines it was read from, that pages render pixel for pixel as
  before, that the file grows by kilobytes only, and that a second run
  skips every page. A page turned by its `/Rotate` previews upright, as
  large as asked.
- `main_test.go`: the window's flow, run headless with `ui.NewTester`,
  and the Fonts dialog's: a collection's font chosen, then another of it,
  a file that cannot be used, the built-in font again, fonts chosen before
  the models load or while others are read, and the choice saved.

The OCR tests use macOS fonts and skip where those are missing. The
converted PDF was also checked in Apple's PDFKit (Preview's engine): its
text extracts and search finds words on every page.

## Maintenance notes

- **go-pdfium bug:** v1.21.3 passes `FPDF_NewXObjectFromPage`'s documents
  to PDFium in the wrong order. `overlayPage` swaps them to compensate
  (see the comment there). Remove the swap once go-pdfium fixes it.
