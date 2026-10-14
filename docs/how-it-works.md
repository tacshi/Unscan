# How Unscan works

This document covers what the [README](../README.md) leaves out: how
Unscan reads a page, turned upright and in reading order, on the GPU
where it can; how it keeps memory flat on long documents; how it makes
the retyped PDF; and what its tests check.

The retyped PDF is called the *text PDF* in the code
(`internal/pdfocr/textpdf.go`, `redraw.go`, `style.go` and `trace.go`)
and on the
command line (`-text`), and so it is here.

## How a page is read

`ocr.Engine.Read` takes a page rendered at 300 DPI through four models:

1. **Orientation.** PP-LCNet_x1_0_doc_ori sees the page as PaddleX shows
   it, its shorter side scaled to 256 pixels (averaged down, as
   `cv2.INTER_AREA` does, so that strokes of text are not skipped) and
   its middle 224×224, and tells whether it is turned 0°, 90°, 180° or
   270° clockwise. On the 316 pages of a picture dictionary, each tried
   in all four turns, it was right 98 times in 100, in about 3 ms a page,
   but took about one upright page in a hundred, of pictures or of small
   type, for turned, at even odds or better. So a page it finds turned
   with even odds or better is read each way it might be: upright, as
   it says, and the other way round; and is kept the way its lines
   across the page read with the most confidence, characters by the
   recognizer's confidence. Read the wrong way, lines run down the page
   or read upside down, with little confidence if at all.
2. **Layout.** PP-DocLayoutV3 finds the page's blocks (paragraphs,
   titles, pictures, tables, headers, footers, page numbers) and the
   order they read in, from the page squeezed to 800×800. It takes about
   half a second on the CPU.
3. **Detection and recognition**, PP-OCRv6's, find and read the lines.
4. **Reading order.** Each line goes in the block that covers most of
   it, or the smallest of those it is all but inside, as a column inside
   a block the model drew round two; the blocks read in the model's
   order, and a block's lines in rows, top to bottom. Lines no block
   covers, which the model missed, go before the first block below them
   that they lie across from, or else at the end. The text joins lines
   in a row of a block with spaces, and rows and blocks with newlines.

A page found turned is redrawn upright for the retyped PDF, and its
`/Rotate` set to show it upright in the searchable PDF, where its scan
stays as it was and the text layer, drawn in the page's own
coordinates, turns with it.

### Core ML

On macOS, the detector and the recognizer run on the GPU and the Neural
Engine through ONNX Runtime's Core ML execution provider, three to four
times as fast as on the CPU: with the layout model, which keeps the CPU
busy meanwhile, a page of a picture dictionary takes about 0.7 seconds on
an M1 Pro, against 1.6 on its CPU and 1.1 before there was a layout model. `onnxruntime_purego` binds the call that adds an execution
provider but does not export it, so `internal/ocr` calls it through
purego from ONNX Runtime's `OrtApi`, given the session options' handle,
which it checks is the first field of `ort.SessionOptions` before
reading it.

Core ML runs a model on the GPU only at the input size it was compiled
for: given inputs of any size, it runs them on the CPU, no faster than
ONNX Runtime. So each size is compiled on its own, with ONNX Runtime's
free dimensions fixed: pages padded with white to sides that are
multiples of 128 pixels, of at most four sizes, as a document's pages
are of a size or two; and lines padded to the narrowest of three widths,
640, 1920 and 3200 pixels, in batches of 8. Each width holds 1 to 2 GB,
as the recognizer's output, the odds of each of its 18,710 classes at
each step, is 48 to 240 MB a batch, and Core ML keeps buffers for it
and the steps before: seven widths took 12 GB in all, three 8 GB on the
GPU alone, and 5.6 GB on the GPU and the Neural Engine, which Core ML is
given, as fast; and the first run of a width after others' is slow, so
three read a page as fast as seven. A Mac of less than 16 GB reads on
the CPU. Compiling a size takes seconds,
so sizes are compiled in the background, one at a time, while ONNX
Runtime reads on the CPU, and each is run once on zeros, as the GPU's
first run of a model takes as long as a few. The recognizer's widths
start compiling as the engine loads; a page's size, as its first page
comes. The compiled models are kept in the cache directory, by the
models' contents, ONNX Runtime's version and the size, as ONNX Runtime
finds a compiled model by its file's name only: a size loads from there
in about 70 ms, though the Neural Engine compiles it again as each run
loads it, which takes 15 to 20 seconds in all. The cache takes about
half a gigabyte. Core ML's sessions run without ONNX Runtime's memory
arena, which, given the recognizer's output of 24 to 240 MB a batch by
its width, allocated anew whenever the width changed, taking up to half
a second. The layout model stays on the CPU: Core ML cannot compile it
as an ML Program (its MaxPool rounds up with padding to the same size),
and runs it three times slower as a neural network. It runs while the
GPU detects and recognizes the page's lines.

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
mostly what the models need, about 3.5 GB on the CPU, of which the
layout model takes 1.2 GB, and 6 GB with Core ML, whose buffers on the
GPU take 2 to 3 GB.

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
   white, with the faint noise around it: what lies within about four
   times the noise's standard deviation of it, which the paper's pixels
   show, from 4 to 16 levels. A clean scan keeps a panel printed in a
   pale tint, which a grainy one's noise would hide.
2. Each line's ink is measured inside its box, and past it where a tight
   box cuts glyphs: the box's edge gives the background, and the pixels
   that differ from it by half the ink's contrast are the ink, its edges
   read to a fraction of a pixel from how dark their pixels are. Where a
   box holds the edges of other lines too, the ink kept is what matches
   the height that the recognized text's glyphs give for its width. Dot
   leaders that a contents page's headings end with, or its page numbers
   start with, are kept as printed: the recognizer reads a few of the dots,
   a dot at times as 。, which set in the heading's face would come out its
   size and weight, and takes others into a line's box without reading
   them. Dots at the end of a line's ink, set apart from the rest by a
   quarter of its height, are the leaders', or by a tenth after a Chinese
   character or before a digit, which a sentence's period does not follow
   or precede; and dots read that have no ink of their own, too faint
   beside the line's, are dropped. Rules along a line, as an underline,
   are told from its text's ink: bands of rows no thicker than an eighth
   of its box's height with a run of ink a fifth longer than the box is
   high, longer than a glyph's stroke. They are kept as printed, and the
   text's ink lies above an underline that runs into its letters' feet,
   so that an underlined word is not set too large. Each word of Latin
   letters or digits is measured too, where the line is first set: its
   strokes, as the line's, and how far they lean, the mean lean of its
   edges that run near upright, which an upright face's round and
   slanting strokes leave about upright, its ink sheared back upright by
   it and read again until little lean is left.
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
   kind, as a heading does, or a line surely heavier, or a first line a
   fifth heavier, as a heading run into its paragraph is. A line too short to
   judge, as a paragraph's last of one character, takes its paragraph's,
   and a paragraph of such lines, as a page number alone, the kind most
   of the document's lines of its script lean to. So does a heading or a
   label, a paragraph of a line or two, that leans to serif less than
   surely, where most of the document's lines are sans-serif: a
   sans-serif face's bold has more contrast than its regular, and leans a
   little to serif. A word of two letters or more is set bold, against
   its own glyphs in its kind's regular face, where it is a fifth heavier
   than the lines of its kind on its page are, as a verb set bold in a
   caption is, and a line's label, of a letter, where it is three tenths
   heavier; a word in a bold line is set regular where it is no more than
   a tenth heavier. A one-letter word within a line is set as the word
   after it.
4. The line is set in its face so that its glyphs' outlines cover its
   ink. Their height gives the font size, which lines within 7% of
   each other on a page share, as a paragraph's do. Glyphs keep their
   shapes: the line is spaced out to the ink's width as books are set.
   Each place its words part at, a space or where CJK characters meet
   Latin ones, is as wide as its gap in the ink: the ink's widest gaps, as
   many as there are places, in order along the line. The rest of a line
   of Latin is taken by a face a tenth wider at most (`Tz`), as the face
   it was printed in may be wider than the text fonts, and by tracking
   its letters out by a twentieth of an em at most; what is left goes
   between CJK characters, three times as much where they meet Latin
   words, and at spaces, or is tracked out further where there are none.
   A line tracked out, as a title's letters are, whose gaps between its
   pieces of ink are a fifth of its height or wider, by their median, is
   tracked out instead, unstretched; and where its pieces lie a third of
   its height apart, one for each letter, each letter is set on its own
   piece, whatever spaces were read, as in a title read "PIC T U R E".
   Where gaps wider than the
   ink is high, by a quarter, part the ink, as a label from what it
   labels, the characters are matched to each stretch by how far their
   outlines span, parting where the text itself leaves a gap, as after a
   comma, but not within a word where the text has a space close by, and
   set on it, and ink no character is matched to, such as a bullet, is
   kept as printed. A line wider than its ink is squeezed (`Tz`); one
   that has nowhere to space, as a letter alone, is stretched; a hyphen
   read between its
   digits, as a page range's, is set as an en dash where that fits the
   ink better. A line whose words lean, by their mean, about half as far
   as italic faces do is set slanted (a shear of its text matrix), as far
   as the document's italic lines lean, by their median, but for its
   label, as "A:", and a word that leans little, as a reply quoted in an
   instruction; a long word in an upright line that leans that far is set
   slanted alone. A word's lean is its edges', by their median, less what
   its letters lean set upright in the text font, as a y's long stroke
   does, measured between the ink's gaps nearest where the word is set. The spacing goes into the
   characters' advances, with a CID for each character and advance, so
   characters abut and text extraction finds no spaces that were not
   read. A line in Kai takes its Latin letters, digits and narrow
   punctuation from the serif face, as books set them in Times beside
   Kai, since LXGW WenKai's own are a casual hand. A document in
   traditional Chinese, which more characters of its alone than of
   simplified's tell, is set in Noto Serif TC and Noto Sans TC, which
   center its punctuation in the em and draw its characters as it
   does, but for characters they lack; its lines are measured and judged
   in the SC faces all the same. On a page in it, characters the
   recognizer reads in the forms of Japanese or simplified Chinese, a
   stroke or two apart, as 説 for 說, are read as traditional Chinese
   draws them, in every output. Each character is set
   in the color of its ink, as a heading in black with its translation in
   blue is, measured across its advance where it is first set, and a word
   of Latin letters or digits in the color most of its characters are,
   as one ink prints a word. Ink that is about black prints black, but
   for gray on a page printed black, as a contents page's section numbers
   are, which stays gray.
5. Lines on plain paper are erased, but for those that type would not
   match, which stay as printed under invisible text, as text on color,
   a pale tint's too, does: a line whose ink is framed, as a letter in a
   box is; one whose glyphs, at the size its ink gives them, would have
   to be squeezed to less than four fifths of their width, and squeezed
   so would not match it, as handwriting or lettering in a picture would
   not; and one the recognizer read with less than 85% confidence. Type
   squeezed matches where the text, set in each text face and stretched
   over the ink, thickened or thinned a pixel at a time to cover as much
   as the ink, correlates with it by 0.72 or more, both blurred by a
   sixteenth of the ink's height: most labels set in a condensed face,
   as a book's figures have them, measure 0.73 or more, and the lettering
   of a picture's signs, calendars and handwriting 0.69 at most. A
   character alone is not judged so, as an X written by hand matches type
   as well as one printed, and a B read as 8 about as well as a B. What
   is left that is not white, a pale tint too, whole, as a panel behind
   a picture or text, is cut out in parts: its line art is traced (see
   6), and the rest goes into images, what is mostly white and ink
   losslessly, the rest as JPEG at 200 DPI at most. Specks of dust are
   dropped, but for rows of four or more alike, level and evenly
   spaced, as dot leaders and dotted lines are, whose dots lie too far
   apart to join; and so are the marks the scan's edges leave, the shadow
   of the paper's edge or the scanner's lid: what lies in the outer half
   of a margin, within 15 mm of the edge, as a line along it or a speck
   within 3 mm of it.
6. Line art, as a figure's boxes, arrows and rules, or text kept as
   printed, is traced: drawn as outlines filled in its ink's color, as
   sharp as the type at any zoom, rather than as an image of the scan's
   pixels. A part is line art where its pixels are ink, about halfway
   from white to black or further, or paper, but for those within two
   pixels of ink, where it fades into the paper, and a few more: a photo,
   a gray panel or a tint stays an image. So does a halftone that a
   black-and-white scan broke into specks, which would be as many
   outlines: a part of more than 64 outlines, most of them round 6
   pixels or fewer. Each piece of ink, its pixels joined edge to edge or
   corner to corner, is filled in its color, that of its pixels a pixel
   from its edges, or of its darkest, for a piece too thin to have any;
   black where about black. A piece whose pixels inside vary by more than
   40 levels, on average, as ink of two colors does, or shades, stays in
   the image. Its outlines run along its pixels' edges, through the
   middle of each, which takes off the stairs of a slanting line or a
   curve, and through the corners where straight runs of three pixels or
   more meet, as a box's do, or where a thin line ends. They are
   simplified to the fewest straight lines within 0.6 pixels of those
   points, by Douglas and Peucker's method, and each corner between two
   is turned by a Bézier curve, as Selinger's Potrace turns them: one
   that passes where the square of the pixel round the corner reaches
   toward the line between the two lines' middles, or sharply, where
   that would take a curve tighter than a cubic bends, or where the
   corner is kept. An outline of fewer than 12 pixels' edges, as a dot's,
   is drawn as its pixels are, and so is one that simplifies to a line,
   as round a hairline slanting. What is traced is erased, with the paler
   pixels along its edges, before what is left of the part goes into an
   image. Outlines run round ink one way and round its holes the other,
   and are filled by the nonzero rule, so that pieces close together
   overlap rather than cancel where their curves meet.

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

- `internal/ocr`: the geometry, turning images and mapping points back,
  OCR of rendered English and Chinese pages, and the order of a page's
  lines by its blocks: a title, a subtitle the layout model missed, two
  columns level with each other inside a block of both, and a page
  number it missed too. A page of two columns reads down the left one,
  then the right, upright and turned each of the three ways, which `Read`
  finds. On macOS, every size Core ML is given compiles, and reads the
  page as the CPU does, faster.
- `internal/ttf`: glyphs, advances and bounds agree with
  `golang.org/x/image/font/sfnt`, and a subset keeps the outlines of the
  glyphs used (composites with their parts) and drops the rest. The fonts
  of a collection read by their English names, and one subsets into a font
  of its own; fonts of PostScript outlines are refused, and fonts are
  embeddable as their `fsType` says.
- `internal/pdfocr` (text PDF): a line set in the font on yellowed paper
  comes back at its size and place, in a loose box or a tight one, with
  the picture beside it kept and the dust dropped; white text on a red
  band stays as printed under invisible text, traced in red with the
  letters holes in it, and so do a letter in a
  box, a line read with little confidence and a word read as more than
  its ink holds; a word set tighter than the font is squeezed to end
  where its ink does. Labels set in a face a third narrower than the
  text faces are set in type, squeezed to end where their ink does,
  while the same ink read as other words, and a letter alone, stay as
  printed. A contents page's heading followed by a dot set
  apart, read as nothing, as a period or as 。, a heading whose 。 read
  has no ink and a page number after a dot are set over their own ink,
  without the dots, which are kept with the rest of their box, while a
  sentence's period and a 。
  set as Chinese sets it stay; a page range read with a hyphen takes the
  en dash it was printed with, and a telephone number keeps its hyphen;
  a section's number in narrow digits is set apart from its title at the
  space between them. A heading in black with its translation in blue is
  set in both, with a color for each run; a word is set in the color most
  of its letters were measured in, a gray section number in gray, and a
  line in one color, its thin 。 measured lighter, in it alone. A
  caption whose verb is bold among lines set regular sets the verb bold
  and the rest regular, and an example set italic after a bold label is
  slanted but for the label. A title tracked out, read as it is or with
  spaces where none are, has each letter set over its own, unstretched;
  a line spaced out where its words part in the ink, a wide gap after a
  section's number, takes the gap there, not tracking; a line two fifths
  wider than its glyphs is set a tenth wider at most, and tracked for
  the rest, or tracked alone where its letters lie apart; and an upright
  line in Arial full of y's stays upright. Text sheared as italic is by 0.12 and 0.2
  measures within 0.04 of that, and upright text about upright; a line
  whose words lean is italic but for its label and a word leaning left,
  and in an upright line only a long word leaning far is. A word
  underlined, the rule running into its letters or apart from them, is
  set at its size, and the rule kept. Dot
  leaders at 300 DPI, too far apart to join, are kept, while
  a speck of dust and three specks in a row are not. A line printed on a
  panel of a pale tint, scanned clean, stays as printed on it, and the
  panel is kept whole, in its tint, as one picture, losslessly. A
  figure's line art at 300 DPI, its edges fading into the paper (a box
  with its shadow, a ring, a slanting arrow, a thin rule and a rule in
  blue), is traced, each piece in its color, and renders over its ink to
  within a pixel at every edge, the box's corners sharp outside and in;
  while a photo, a gray panel and a halftone of specks beside it stay
  images. A line in
  traditional Chinese is set in the TC faces, which center its 。, and judged in the
  SC ones, and a line in simplified Chinese in the SC faces; traditional
  Chinese is told from simplified, and from Japanese, whose 説 is left
  as read on its pages. The marks of a scan's
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
  too, and a bold Song heading as slanted as Kai's least is not. In a
  sans-serif document, a heading leaning a little to serif, a page number
  and a line whose ink was not found are sans-serif; in a serif one, a
  heading leaning a little to sans-serif stays so. A paragraph mostly
  regular keeps a first line a fifth heavier bold, but not a later one,
  unless surely heavier. Gray ink keeps its
  gray on a page printed black, and is set black on one printed lighter.
  A line
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
  large as asked. Pages fed into the scanner sideways and upside down,
  one turned by its `/Rotate` too, are found turned, read upright, shown
  upright in the searchable PDF, which reads upright again, and redrawn
  upright in the text PDF, and the scans are not written again.
- `main_test.go`: the window's flow, run headless with `ui.NewTester`:
  among it, the arrows turn the page after the line between the pages
  compared is dragged, and move the line once Tab reaches it; a page's
  number typed in place of the page shown's goes there, within the
  document, on Enter, and nowhere on Escape or a click elsewhere; New leaves
  a document for the welcome, keeping the history, where a PDF dropped
  while another is read waits, its progress showing; and the Settings
  dialog's: a collection's font chosen, then another of
  it, a file that cannot be used, the built-in font again, fonts chosen
  before the models load or while others are read, and the choice saved;
  updates checked for and installed without asking, the latter only with
  the former, a check now, and a copy that cannot update itself.

The OCR tests use macOS fonts and skip where those are missing. The
converted PDF was also checked in Apple's PDFKit (Preview's engine): its
text extracts and search finds words on every page.

## Maintenance notes

- **go-pdfium bug:** v1.21.3 passes `FPDF_NewXObjectFromPage`'s documents
  to PDFium in the wrong order. `overlayPage` swaps them to compensate
  (see the comment there). Remove the swap once go-pdfium fixes it.
- **Hardened runtime:** wazero compiles PDFium's WebAssembly to machine
  code in memory mapped without `MAP_JIT`, which the hardened runtime of a
  Developer ID build runs only under
  `com.apple.security.cs.allow-unsigned-executable-memory`, in
  `entitlements.plist`. Without it the app is killed with "Code Signature
  Invalid" as PDFium starts, about two seconds after launch, while ad-hoc
  development builds, without the hardened runtime, work.
  `scripts/build-mac-release.sh` checks the app is signed with it. Should
  wazero map its code with `MAP_JIT`, `com.apple.security.cs.allow-jit`
  would do instead.
