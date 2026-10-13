# Unscan

A [MyGo](https://github.com/egoist/mygo) prototype that turns scanned PDFs
into searchable ones, entirely on the user's machine. PaddleOCR's
**PP-OCRv6 small** models read each page, and the text goes over the page
as an invisible layer, the way OCRmyPDF does. The original pages are kept
as they are, and the scans are not encoded again.

It also makes a **text PDF**: each scanned page redrawn with its text set
in real fonts where it was printed, at the size it was printed, in the
kind and weight it was printed in (Noto Serif SC or Noto Sans SC, regular
or bold, or LXGW WenKai for Kai, the brush-written face of Chinese prefaces
and running heads), on white paper, so that the page reads like one exported from a
word processor. What else the page shows (pictures, rules, logos) is kept
as images under the text. Text printed on color or over a picture, such as
a cover's title, stays as printed, since retyping it would deface the
design, with invisible text over it that keeps it searchable. Its fonts
can be the user's own instead (see [Your own fonts](#your-own-fonts)).

Everything builds with `CGO_ENABLED=0`, as MyGo requires:

| Part | How it runs without cgo |
|---|---|
| OCR models | [ONNX Runtime](https://onnxruntime.ai) 1.27.1, loaded at run time with [purego](https://github.com/ebitengine/purego) through [onnxruntime_purego](https://github.com/getcharzp/onnxruntime_purego) |
| PDF rendering and editing | [PDFium](https://pdfium.googlesource.com/pdfium/) compiled to WebAssembly, run by [wazero](https://wazero.io) through [go-pdfium](https://github.com/klippa-app/go-pdfium) |
| Pre- and post-processing | Go ports of PaddleOCR's text detection (DB) and recognition (CTC) steps |
| Text layer | A small PDF writer with a "glyphless" font, as Tesseract uses |
| Text PDF | The same writer, a TrueType subsetter (`internal/ttf`) that embeds just the glyphs used, and PDFium to put the pages together |

## Setup

```sh
go run ./cmd/fetchdeps                  # models, fonts and this machine's ONNX Runtime (~145 MB)
go run ./cmd/fetchdeps -platforms all   # every platform, for cross-platform builds
```

Every download is pinned (Hugging Face commit, ONNX Runtime release, font
release) and checked against its SHA-256. The files go into `resources/`,
which `.gitignore` leaves out:

```
resources/
├── models/det.onnx            PP-OCRv6_small_det (9.9 MB, Apache-2.0)
├── models/rec.onnx            PP-OCRv6_small_rec (21 MB, Apache-2.0)
├── models/rec_dict.txt        its 18,708 characters, from inference.yml
├── fonts/NotoSerifSC-{Regular,Bold}.ttf  the text PDF's serif faces (15 MB each, OFL-1.1)
├── fonts/NotoSansSC-{Regular,Bold}.ttf   and its sans-serif ones (11 MB each, OFL-1.1)
├── fonts/LXGWWenKai-Regular.ttf          and its Kai face, v1.522 (26 MB, OFL-1.1)
├── fonts/NotoSerifSC-OFL.txt, NotoSansSC-OFL.txt, LXGWWenKai-OFL.txt  their licenses
└── darwin-arm64/libonnxruntime.dylib  (or linux-*/libonnxruntime.so, windows-*/onnxruntime.dll)
```

`mygo build` merges the platform's directory into the app's resources.

## Run

```sh
go tool mygo dev                        # the app, with live reload
go run ./cmd/unscan -txt out.txt -text out-text.pdf testdata/sample-scan.pdf   # the command line version
go tool mygo build                      # build/darwin-arm64/Unscan.app and a .dmg
```

### macOS builds

```sh
scripts/build-mac-debug.sh [--open]   # build/debug/darwin-arm64/Unscan.app, with the native UI inspector
scripts/build-mac-release.sh          # tests, then build/release/darwin-arm64/Unscan.app and its .dmg
```

Both fetch any missing models and runtime first. The release script signs
ad hoc unless `MACOS_SIGNING_IDENTITY` names a Developer ID. With
`NOTARY_PROFILE`, the keychain profile of `xcrun notarytool
store-credentials`, it also notarizes the disk image and staples it. The
debug build skips the disk image. Both target Apple silicon only.

The app takes a PDF from **Choose PDF…**, **File › Open…**, a drop
anywhere in its window or `open -a Unscan scan.pdf`, even while its
models are still loading, and shows how many pages it has read, and about
how long the rest will take, in the window and on its Dock icon. It then
shows the document a page at a time, as the **Searchable PDF**, the
**Retyped PDF** (the text PDF) or the **Text**, and **Save…** saves the
one shown; the arrow keys turn the pages. Pages that already have text,
such as born-digital ones, are left alone, in the text PDF too.

### Your own fonts

The text PDF can be set in the user's own fonts instead of the built-in
ones: TrueType fonts, a `.ttf` file or one font of a `.ttc` collection,
such as macOS's Songti SC or Times, of any units per em. In the app,
**Unscan › Settings…** (⌘,) chooses a font file for each face: serif and
sans-serif, regular and bold, and Kai. Of a collection, the font named
Regular or Bold, as the face is, is chosen, and a menu chooses another.
The choice is kept in `fonts.json` in the app's user data folder, and
applies to the PDFs read from then on. The command line version takes
the fonts as flags:

```sh
go run ./cmd/unscan -text out-text.pdf -sans /System/Library/Fonts/Supplemental/Arial.ttf \
    -serif /System/Library/Fonts/Supplemental/Songti.ttc -serif-bold /System/Library/Fonts/Supplemental/Songti.ttc#1 scan.pdf
```

where `#n` chooses a collection's font n, from 0. A kind given one weight
only is set in it in both. Lines are measured and judged against the
built-in fonts all the same, as the judging is tuned to their faces (see
below), and the user's fonts only set them. The built-in font of a face
sets the characters its user's font lacks, such as Chinese in a Latin
font, so Times or Arial can set the Latin of a Chinese book. Fonts of
PostScript outlines (CFF), as `.otf` files and Hiragino's mostly are, are
refused, and so are fonts whose licenses forbid embedding them (by their
OS/2 `fsType`), as Songti SC Black's does: their subsets go into the PDFs.

### Long documents

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
are saved; the app keeps them in a temporary directory until they are
saved where the user chooses, and the command line version writes them
beside where they go and moves them there once whole. PDFium keeps what
it reads of a document, the scan of every page it renders, until the
document is closed, so the pipeline opens it anew every 32 pages. Its
WebAssembly memory is mapped whole at the start, on macOS and Linux,
and filled in as it grows, rather than grown as a Go slice, which is
copied each time and leaves the copies to the garbage collector. Only
the pages along the pipeline are in memory at once, so while pages are
read, memory does not grow with the document: it is mostly what the
models need, about 3 GB.

Adding the text layer at the end has PDFium load every page once more,
keeping its scan until the document is closed, in memory that is 2 GB
at most. So the pages of an input bigger than 512 MB have their layer
added in batches, each saved to a temporary file that is opened anew for
the next, which lets go of the batch's scans; the saves take seconds.

### How the text PDF is made

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

## Tests

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

## Layout

```
main.go                  the app: state, actions, effects (dialogs, the menu bar, the Dock)
view.go                  the window: the PDF asked for, the progress, the pages in each form
fonts.go                 the user's fonts for retyped PDFs: the Fonts dialog, and the choice saved
cmd/unscan/              the command line version
cmd/fetchdeps/           downloads and checks the models and runtimes
internal/ocr/            PP-OCRv6: detection, cropping, recognition, reading order
internal/pdfocr/         rendering, the text layer, grafting it onto the PDF, the text PDF, and previews
internal/ttf/            TrueType parsing (fonts and collections) and subsetting, for the text PDF
internal/testscan/       synthetic scans and scanned PDFs for tests
```

## Known limits and next steps

- **Intel Macs:** Microsoft ships no ONNX Runtime build for them, so
  `darwin/amd64` builds compile but find no runtime. Build one, or read
  pages with Apple Vision on macOS instead (for example through
  [`tmc/apple/vision`](https://pkg.go.dev/github.com/tmc/apple/vision),
  which also uses purego).
- **Reading order** comes from line positions only, so multi-column pages
  interleave in the text output, though the searchable PDF is unaffected.
  A layout model (PP-DocLayoutV3, ONNX) would order them.
- **Orientation:** there is no page or text-line orientation classifier
  yet, so pages scanned upside down read poorly. PaddleOCR's
  `PP-LCNet_x1_0_doc_ori` (ONNX, 6.5 MB) fits the same pipeline.
- **Speed:** OCR takes about 2 seconds a page of dense text on an M1 Pro,
  on the CPU (see Long documents). The CoreML or DirectML execution
  providers are untried; `onnxruntime_purego` binds neither yet.
- **Characters outside the BMP** (such as emoji) are written as two UTF-16
  code units, which some viewers extract as two characters.
- **Text PDF:** a line is set in one face, serif, sans-serif or Kai,
  regular or bold, so a bold phrase within a regular line comes out as its line
  is, and italics come out upright. Medium weights come out bold, and
  display faces, such as a title's condensed one, are squeezed into the
  nearest of the five; FangSong comes out Kai, and bold Kai, which LXGW
  WenKai has no face for, regular. Calligraphic lettering on covers, as a
  publisher's name, often comes out Kai. A face used only for bold lines in a document, with
  none of its lines regular, comes out regular, as there is no body text
  of its kind to judge them against. OCR mistakes show, where the searchable PDF hides them under the scan, and characters
  the recognizer misses (such as some dashes) are erased with their line.
  Vertical text is set turned, not stacked. Covers and other designed pages
  come out mostly as images, since their text is on color.
- **Your own fonts:** fonts of PostScript outlines (CFF) cannot be used, as
  `internal/ttf` subsets TrueType outlines only; a CFF subsetter, with
  `FontFile3`, would take them. Fonts chosen apply to the PDFs read after:
  setting the text PDF again in them means reading the scan again, though
  the lines measured are the same whatever the fonts; keeping them would
  retype the PDF in seconds. A kind given in one weight sets its bold
  lines in that weight, as no bold is synthesized.
- **go-pdfium bug:** v1.21.3 passes `FPDF_NewXObjectFromPage`'s documents
  to PDFium in the wrong order. `overlayPage` swaps them to compensate
  (see the comment there). Remove the swap once go-pdfium fixes it.

## Licenses

PP-OCRv6 models: Apache-2.0. ONNX Runtime: MIT. PDFium: Apache-2.0 / BSD.
Noto Serif SC, Noto Sans SC and LXGW WenKai: SIL Open Font License 1.1,
which `fonts/NotoSerifSC-OFL.txt`, `fonts/NotoSansSC-OFL.txt` and
`fonts/LXGWWenKai-OFL.txt` carry into the app; subsets of them are
embedded in text PDFs, as the license allows. Subsets of the user's own
fonts are embedded under those fonts' licenses, which the user answers
for; fonts that forbid embedding are refused.
go-pdfium, onnxruntime_purego, purego: MIT / Apache-2.0. Check the
dependencies' own terms before you ship.
