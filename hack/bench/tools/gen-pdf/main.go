// gen-pdf: generate a deterministic, valid PDF 1.4 of a target size.
// No external PDF libs — bytes written by hand.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
)

var words = []string{
	"lorem", "ipsum", "dolor", "sit", "amet", "consectetur", "adipiscing", "elit",
	"sed", "do", "eiusmod", "tempor", "incididunt", "ut", "labore", "et", "dolore",
	"magna", "aliqua", "enim", "ad", "minim", "veniam", "quis", "nostrud",
	"exercitation", "ullamco", "laboris", "nisi", "aliquip", "ex", "ea", "commodo",
	"consequat", "duis", "aute", "irure", "in", "reprehenderit", "voluptate",
	"velit", "esse", "cillum", "eu", "fugiat", "nulla", "pariatur", "excepteur",
	"sint", "occaecat", "cupidatat", "non", "proident", "sunt", "culpa", "qui",
	"officia", "deserunt", "mollit", "anim", "id", "laborum", "the", "quick",
	"brown", "fox", "jumps", "over", "lazy", "dog", "pack", "my", "box", "with",
	"five", "dozen", "liquor", "jugs", "how", "vexingly", "daft", "zebras", "jump",
	"waltz", "bad", "nymph", "for", "quartz", "sphinx", "of", "black", "judge",
	"glyph", "vex", "dwarf", "fjord", "bank", "big", "wave", "cast", "shadow",
	"onto", "ancient", "stones", "beneath", "tall", "grey", "cloud", "distant",
	"mountain", "silver", "river", "meanders", "through", "green", "valley",
	"warm", "wind", "carries", "faint", "smell", "of", "pine", "and", "smoke",
	"bright", "star", "hangs", "low", "east", "while", "moon", "climbs", "west",
	"soft", "steady", "rain", "falls", "roof", "small", "wooden", "cabin", "fire",
	"crackles", "hearth", "book", "lies", "open", "on", "table", "clock", "ticks",
	"quiet", "hours", "away", "kettle", "whistles", "stove", "steam", "curls",
	"cool", "air", "cat", "sleeps", "curled", "chair", "dream", "chasing",
	"shadows", "along", "wall", "old", "letters", "tied", "ribbon", "sit", "desk",
	"unread", "long", "years", "candle", "flickers", "beside", "them", "casting",
	"warm", "light", "across", "page", "someone", "wrote", "once", "with",
	"careful", "hand", "hoping", "would", "reach", "kind", "reader", "someday",
}

func main() {
	sizeStr := flag.String("size", "", "target size (e.g. 100MB, 50KB, 1MiB, or bytes)")
	outPath := flag.String("out", "", "output pdf path")
	seed := flag.Int64("seed", 42, "random seed")
	mode := flag.String("mode", "text", "text | scan  (scan emits image-XObject pages ~2 MB each — mirrors a real 100 MB scanned PDF)")
	flag.Parse()

	if *sizeStr == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: gen-pdf -size <human> -out <file> [-seed <int>] [-mode text|scan]")
		os.Exit(2)
	}
	target, err := parseSize(*sizeStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "parse size:", err)
		os.Exit(1)
	}

	rng := rand.New(rand.NewSource(*seed))
	var (
		data  []byte
		pages int
	)
	switch *mode {
	case "text", "":
		data, pages = buildPDF(rng, target)
	case "scan":
		data, pages = buildScanPDF(rng, target)
	default:
		fmt.Fprintln(os.Stderr, "-mode must be text or scan")
		os.Exit(2)
	}
	if err := os.WriteFile(*outPath, data, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "wrote %s size=%d target=%d pages=%d mode=%s\n", *outPath, len(data), target, pages, *mode)
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	// Order longest-suffix first.
	mults := []struct {
		suf string
		n   int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"B", 1},
	}
	for _, m := range mults {
		if strings.HasSuffix(s, m.suf) {
			num := strings.TrimSpace(strings.TrimSuffix(s, m.suf))
			v, err := strconv.ParseFloat(num, 64)
			if err != nil {
				return 0, err
			}
			return int64(v * float64(m.n)), nil
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

// buildPDF constructs a PDF 1.4 with N pages of packed lorem text, then trims
// the final page's content stream to land within ±1% of target.
func buildPDF(rng *rand.Rand, target int64) ([]byte, int) {
	// Empirically ~15KB text per page; overhead per page ~120 bytes for
	// object wrappers + content-stream header. We over-produce then trim.
	const pageTextBytes = 15 * 1024
	estOverhead := int64(2048) // header + catalog + tree + xref frame
	estPerPage := int64(pageTextBytes + 200)
	pages := int((target-estOverhead)/estPerPage) + 1
	if pages < 1 {
		pages = 1
	}

	// Generate deterministic page text streams.
	streams := make([]string, pages)
	for i := 0; i < pages; i++ {
		streams[i] = makeStream(rng, pageTextBytes)
	}

	// Assemble, measure, trim last page to hit target.
	for iter := 0; iter < 200; iter++ {
		buf := assemble(streams)
		diff := int64(len(buf)) - target
		if abs(diff)*100 <= target { // within 1%
			return buf, pages
		}
		if diff < 0 {
			// Under target: add pages.
			add := int((-diff)/estPerPage) + 1
			for k := 0; k < add; k++ {
				streams = append(streams, makeStream(rng, pageTextBytes))
			}
			pages = len(streams)
			continue
		}
		// Over target: shrink last page's stream. If already tiny, drop the page.
		last := streams[len(streams)-1]
		if int64(len(last)) > diff+64 {
			streams[len(streams)-1] = trimStream(last, int64(len(last))-diff)
			// One more pass will re-measure and likely accept.
			buf2 := assemble(streams)
			return buf2, len(streams)
		}
		if len(streams) > 1 {
			streams = streams[:len(streams)-1]
			pages = len(streams)
			continue
		}
		// One page and still over? Trim aggressively.
		streams[0] = trimStream(streams[0], int64(len(streams[0]))-diff)
		buf2 := assemble(streams)
		return buf2, 1
	}
	return assemble(streams), pages
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// makeStream returns a raw PDF text content stream body (no length header)
// sized to approximately n bytes, using packed BT/Tj/ET operators.
func makeStream(rng *rand.Rand, n int) string {
	var b strings.Builder
	b.Grow(n + 256)
	b.WriteString("BT\n/F1 10 Tf\n50 800 Td\n")
	// Line y-decrement handled by successive Td offsets; we just emit many Tj
	// with newline moves via T* (0 -12 Td). Wrap words to ~80 chars.
	line := make([]string, 0, 16)
	lineLen := 0
	for b.Len() < n {
		w := words[rng.Intn(len(words))]
		if lineLen+len(w)+1 > 80 {
			b.WriteString("(")
			b.WriteString(strings.Join(line, " "))
			b.WriteString(") Tj\n0 -12 Td\n")
			line = line[:0]
			lineLen = 0
		}
		line = append(line, w)
		lineLen += len(w) + 1
	}
	if len(line) > 0 {
		b.WriteString("(")
		b.WriteString(strings.Join(line, " "))
		b.WriteString(") Tj\n")
	}
	b.WriteString("ET\n")
	return b.String()
}

// trimStream shortens a stream to approximately target bytes while keeping it
// syntactically valid (drops complete Tj lines from the end, preserves ET).
func trimStream(s string, target int64) string {
	if int64(len(s)) <= target {
		return s
	}
	// Split off trailing ET so we can re-attach it.
	end := "ET\n"
	body := strings.TrimSuffix(s, end)
	// Drop lines until under target - len(end).
	limit := int(target) - len(end)
	if limit < 32 {
		limit = 32
	}
	if len(body) <= limit {
		return s
	}
	// Find a safe cut: newline at or before limit.
	cut := strings.LastIndex(body[:limit], "\n")
	if cut < 0 {
		cut = limit
	}
	return body[:cut+1] + end
}

// buildScanPDF emits an image-only PDF that mirrors a real "scanned invoice"
// shape: ~50 pages of a full-page grayscale Image XObject each, so a 100 MB
// target lands as ~50 image pages rather than 6000 text pages. Deterministic
// per seed. The pipeline routes it through qpdf → pdftotext (finds no text)
// → OCR — exercising the honest ingest path.
func buildScanPDF(rng *rand.Rand, target int64) ([]byte, int) {
	// Rough page budget: 2 MiB per page → 50 pages at 100 MB.
	// Image geometry: W × H grayscale 8-bit → raw bytes = W*H.
	const imgW = 1500
	perPageBytes := int64(2 * 1024 * 1024)
	if target < 4*1024*1024 {
		perPageBytes = target / 4
		if perPageBytes < 128*1024 {
			perPageBytes = 128 * 1024
		}
	}
	imgH := int(perPageBytes / imgW)
	if imgH < 512 {
		imgH = 512
	}
	imgBytesPerPage := int64(imgW * imgH)

	const overhead = int64(4096)
	pages := int((target-overhead)/(imgBytesPerPage+256)) + 1
	if pages < 1 {
		pages = 1
	}

	imgs := make([][]byte, pages)
	for i := 0; i < pages; i++ {
		imgs[i] = makeScanImage(rng, imgW, imgH)
	}
	buf := assembleScan(imgs, imgW, imgH)
	// One-shot trim of the last image if we're materially over target.
	if int64(len(buf))-target > target/50 && pages > 1 {
		imgs = imgs[:pages-1]
		pages--
		buf = assembleScan(imgs, imgW, imgH)
	}
	return buf, pages
}

// makeScanImage returns a W*H grayscale byte buffer. Content is a mix of
// bright background (~220) with periodic darker bands and pseudo-random
// speckle — enough visual structure that a viewer sees "paper", no OCR
// pretense.
func makeScanImage(rng *rand.Rand, w, h int) []byte {
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		band := byte(210)
		if y%37 < 3 {
			band = 60
		}
		row := out[y*w : (y+1)*w]
		for x := 0; x < w; x++ {
			p := int(band) + rng.Intn(30) - 15
			if p < 0 {
				p = 0
			} else if p > 255 {
				p = 255
			}
			row[x] = byte(p)
		}
	}
	return out
}

// assembleScan builds a PDF whose page tree references one Image XObject per
// page. Object layout:
//
//	1 Catalog · 2 Pages · 3 Font (unused but harmless)
//	4..4+N-1  Page objs
//	4+N..4+2N-1  Image XObjects
//	4+2N..4+3N-1  Content streams (draw the image)
func assembleScan(imgs [][]byte, w, h int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")

	n := len(imgs)
	pageStart := 4
	imgStart := pageStart + n
	contStart := imgStart + n
	offsets := make(map[int]int)

	writeDict := func(obj int, body string) {
		offsets[obj] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", obj, body)
	}
	writeStream := func(obj int, dictBody string, stream []byte) {
		offsets[obj] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nstream\n", obj, dictBody)
		buf.Write(stream)
		buf.WriteString("\nendstream\nendobj\n")
	}

	writeDict(1, "<< /Type /Catalog /Pages 2 0 R >>")

	var kids strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", pageStart+i)
	}
	writeDict(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d /MediaBox [0 0 612 792] >>", kids.String(), n))
	writeDict(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	for i := 0; i < n; i++ {
		body := fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 3 0 R >> /XObject << /Im1 %d 0 R >> >> /Contents %d 0 R >>",
			imgStart+i, contStart+i)
		writeDict(pageStart+i, body)
	}
	for i, img := range imgs {
		dict := fmt.Sprintf(
			"<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /DeviceGray /BitsPerComponent 8 /Length %d >>",
			w, h, len(img))
		writeStream(imgStart+i, dict, img)
	}
	for i := 0; i < n; i++ {
		// Draw XObject scaled to full Letter page.
		content := []byte("q\n612 0 0 792 0 0 cm\n/Im1 Do\nQ\n")
		dict := fmt.Sprintf("<< /Length %d >>", len(content))
		writeStream(contStart+i, dict, content)
	}

	xrefOff := buf.Len()
	size := contStart + n
	fmt.Fprintf(&buf, "xref\n0 %d\n", size)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < size; i++ {
		off, ok := offsets[i]
		if !ok {
			buf.WriteString("0000000000 65535 f \n")
			continue
		}
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", size, xrefOff)
	return buf.Bytes()
}

func assemble(streams []string) []byte {
	var buf bytes.Buffer
	// PDF header. The second line's 8-bit comment tells tools the file is binary.
	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")

	// Reserve object numbers:
	// 1 = Catalog, 2 = Pages, 3 = Font, 4..4+N-1 = Page objs, then N content streams.
	nPages := len(streams)
	pageObjStart := 4
	contentObjStart := pageObjStart + nPages

	offsets := make(map[int]int) // obj num → byte offset

	write := func(objNum int, body string) {
		offsets[objNum] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", objNum, body)
	}

	// Catalog.
	write(1, "<< /Type /Catalog /Pages 2 0 R >>")
	// Pages tree.
	var kids strings.Builder
	for i := 0; i < nPages; i++ {
		if i > 0 {
			kids.WriteByte(' ')
		}
		fmt.Fprintf(&kids, "%d 0 R", pageObjStart+i)
	}
	write(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d /MediaBox [0 0 612 792] >>", kids.String(), nPages))
	// Font.
	write(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	// Page objects.
	for i := 0; i < nPages; i++ {
		body := fmt.Sprintf("<< /Type /Page /Parent 2 0 R /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentObjStart+i)
		write(pageObjStart+i, body)
	}
	// Content stream objects.
	for i, s := range streams {
		offsets[contentObjStart+i] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n<< /Length %d >>\nstream\n%sendstream\nendobj\n",
			contentObjStart+i, len(s), s)
	}

	// xref.
	xrefOff := buf.Len()
	totalObjs := contentObjStart + nPages // last obj num + 1 - 1? Actually last obj number is contentObjStart+nPages-1; xref size = last+1.
	size := totalObjs
	fmt.Fprintf(&buf, "xref\n0 %d\n", size)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i < size; i++ {
		off, ok := offsets[i]
		if !ok {
			buf.WriteString("0000000000 65535 f \n")
			continue
		}
		fmt.Fprintf(&buf, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", size, xrefOff)
	return buf.Bytes()
}
