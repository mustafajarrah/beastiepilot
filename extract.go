package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"html"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// This file turns the files under SOURCE_PATH, DOCS_SOURCE and RAG_SOURCE into
// plain text cut into excerpts, ready to be embedded for the index.

const (
	maxFileBytes = 256 << 20 // files larger than this on disk are not read
	maxTextBytes = 32 << 20  // nor are files holding more text than this

	chunkTarget = 1000 // bytes of text an excerpt aims for
	chunkMax    = 1600 // and never exceeds
	chunkMin    = 40   // shorter scraps, such as a lone heading, are dropped

	pdfTimeout = 2 * time.Minute // for converting one PDF
)

// docKind is a file format the index can read.
type docKind int

const (
	kindNone docKind = iota // not indexed
	kindText                // plain text, Markdown and the like
	kindMan                 // roff manual page source
	kindHTML
	kindEPUB
	kindPDF // converted by pdftotext, when it is installed
)

var textExtensions = wordSet(".txt .text .md .markdown .rst .adoc .asciidoc .org .ascii")

// manualSections are the manual sections taken from a source tree: user
// commands, devices, file formats, miscellanea and system administration.
// Those are what a shell command is written from; the library and kernel
// sections, like the code itself, are not.
const manualSections = "14578"

// classify decides from a file's name how it should be read. gz reports a
// ".gz" suffix around the format.
func classify(name string) (kind docKind, gz bool) {
	base := strings.ToLower(filepath.Base(name))
	if strings.HasPrefix(base, ".") {
		return kindNone, false
	}
	if strings.HasSuffix(base, ".gz") {
		gz, base = true, strings.TrimSuffix(base, ".gz")
	}
	switch ext := filepath.Ext(base); {
	case ext == "", textExtensions[ext]:
		// A name without an extension (README, UPDATING) is taken for text;
		// extract checks that the content agrees.
		return kindText, gz
	case len(ext) == 2 && ext[1] >= '1' && ext[1] <= '9':
		return kindMan, gz
	case ext == ".html", ext == ".htm", ext == ".xhtml":
		return kindHTML, gz
	case ext == ".epub" && !gz:
		return kindEPUB, false
	case ext == ".pdf" && !gz:
		return kindPDF, false
	}
	return kindNone, false
}

// docTitle names a file for the model and the user: "zfs(8)" for a manual
// page, the file name for anything else.
func docTitle(path string) string {
	base := filepath.Base(path)
	if strings.HasSuffix(strings.ToLower(base), ".gz") {
		base = base[:len(base)-len(".gz")]
	}
	if kind, _ := classify(path); kind == kindMan {
		ext := filepath.Ext(base)
		return strings.TrimSuffix(base, ext) + "(" + ext[1:] + ")"
	}
	return base
}

// manualSection returns the section digit in the name of a manual page.
func manualSection(path string) rune {
	title := docTitle(path) // name(8)
	return rune(title[len(title)-2])
}

// candidate is a file selected for the index.
type candidate struct {
	path  string
	size  int64
	mtime int64 // modification time, in nanoseconds since the epoch
	kind  docKind
	gz    bool
}

// collect lists the files under root that the index can read, in a stable
// order. root may also be a single file. With manualOnly, only manual pages of
// the sections in manualSections are taken. ignored counts the files left out
// because of their type or size; it stays zero with manualOnly, where nearly
// every file is left out by design.
func collect(root string, manualOnly bool) (files []candidate, ignored int, err error) {
	// WalkDir does not follow a symbolic link, even at the root.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, 0, err
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			ignored++ // an unreadable directory or entry
			return nil
		}
		if d.IsDir() {
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir // .git and friends
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		kind, gz := classify(path)
		if manualOnly && (kind != kindMan || !strings.ContainsRune(manualSections, manualSection(path))) {
			return nil
		}
		if kind == kindPDF && rendition(path) {
			return nil
		}
		info, err := d.Info()
		if kind == kindNone || err != nil || info.Size() > maxFileBytes {
			if !manualOnly {
				ignored++
			}
			return nil
		}
		files = append(files, candidate{path, info.Size(), info.ModTime().UnixNano(), kind, gz})
		return nil
	})
	return files, ignored, err
}

// rendition reports whether the PDF at path is another rendering of HTML
// documentation kept beside it, the way the FreeBSD documentation packages
// install it: books/handbook/handbook_en.pdf next to the handbook's HTML
// pages. Reading both would put every passage in the index twice.
func rendition(path string) bool {
	dir := filepath.Dir(path)
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	cut := strings.LastIndexByte(name, '_')
	if cut < 0 || name[:cut] != filepath.Base(dir) {
		return false
	}
	if lang := name[cut+1:]; len(lang) < 2 || len(lang) > 5 {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
		return true
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, e.Name(), "index.html")); err == nil {
				return true
			}
		}
	}
	return false
}

var (
	errNotText   = errors.New("not a text file")
	errTooLarge  = errors.New("too much text")
	errNoPDFTool = errors.New("pdftotext is not installed")
)

// extractor reads files as text.
type extractor struct {
	pdftotext string // path of the pdftotext program, or "" if not installed
}

func newExtractor() extractor {
	tool, _ := exec.LookPath("pdftotext")
	return extractor{pdftotext: tool}
}

// extract returns the text of the file c, cleaned for display and embedding.
func (x extractor) extract(ctx context.Context, c candidate) (string, error) {
	var text string
	var err error
	switch c.kind {
	case kindEPUB:
		text, err = epubText(c.path)
	case kindPDF:
		text, err = x.pdfText(ctx, c.path)
	default:
		var data []byte
		if data, err = readText(c.path, c.gz); err != nil {
			break
		}
		text = string(data)
		switch c.kind {
		case kindMan:
			if !looksLikeManual(text) {
				return "", errNotText
			}
			text = manToText(text)
		case kindHTML:
			text = htmlToText(text)
		}
	}
	if err != nil {
		return "", err
	}
	return cleanText(text), nil
}

// readText reads a file, decompressing it if gz is set, and checks that it
// holds text rather than binary data.
func readText(path string, gz bool) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var r io.Reader = f
	if gz {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		r = zr
	}
	data, err := io.ReadAll(io.LimitReader(r, maxTextBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTextBytes {
		return nil, errTooLarge
	}
	if !looksLikeText(data) {
		return nil, errNotText
	}
	return data, nil
}

// looksLikeText reports whether data starts like text: no NUL bytes and
// hardly any control characters.
func looksLikeText(data []byte) bool {
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	control := 0
	for _, b := range head {
		switch {
		case b == 0:
			return false
		case b < 0x20 && b != '\n' && b != '\t' && b != '\r' && b != '\f':
			control++
		}
	}
	return control*50 <= len(head)
}

// looksLikeManual reports whether text is roff manual page source: it must
// reach a macro that opens a page with nothing before it but blank lines,
// other roff control lines (comments and requests) and the macro definitions
// that generated pages start with. Many files that are not manual pages have
// names ending in a digit.
func looksLikeManual(text string) bool {
	inDefinition := false
	for n := 0; n < 2000 && text != ""; n++ {
		var line string
		line, text, _ = strings.Cut(text, "\n")
		line = strings.TrimSpace(line)
		switch {
		case inDefinition:
			inDefinition = line != ".."
			continue
		case line == "":
			continue
		case !roffControl(line):
			return false
		}
		for _, macro := range []string{".Dd", ".Dt", ".TH", ".Sh", ".SH", ".so "} {
			if strings.HasPrefix(line, macro) {
				return true
			}
		}
		inDefinition = strings.HasPrefix(line, ".de") || strings.HasPrefix(line, ".am") || strings.HasPrefix(line, ".ig")
	}
	return false
}

// roffControl reports whether a line of roff source is a request, a macro
// call, a comment or a block brace, as opposed to text to be printed.
func roffControl(line string) bool {
	return line != "" && (line[0] == '.' || line[0] == '\'' ||
		strings.HasPrefix(line, `\"`) || strings.HasPrefix(line, `\{`) || strings.HasPrefix(line, `\}`))
}

// epubText returns the text of the XHTML documents inside an EPUB, which is a
// zip archive. Their order within the archive is kept; for searching it does
// not matter.
func epubText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()

	var b strings.Builder
	for _, f := range zr.File {
		switch strings.ToLower(filepath.Ext(f.Name)) {
		case ".xhtml", ".html", ".htm":
		default:
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxTextBytes+1))
		rc.Close()
		if err != nil {
			return "", err
		}
		b.WriteString(htmlToText(string(data)))
		b.WriteString("\n\n")
		if b.Len() > maxTextBytes {
			return "", errTooLarge
		}
	}
	return b.String(), nil
}

// cappedBuffer keeps what is written to it up to max bytes and remembers
// whether there was more. It never fails, so the writer is not left blocked.
type cappedBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.over = true
		b.Buffer.Write(p[:room])
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// pdfText converts a PDF with pdftotext, from the poppler utilities. The
// standard library cannot read PDF.
func (x extractor) pdfText(ctx context.Context, path string) (string, error) {
	if x.pdftotext == "" {
		return "", errNoPDFTool
	}
	ctx, cancel := context.WithTimeout(ctx, pdfTimeout)
	defer cancel()

	out := &cappedBuffer{max: maxTextBytes}
	// path is absolute, so it cannot be mistaken for an option.
	cmd := exec.CommandContext(ctx, x.pdftotext, "-q", "-enc", "UTF-8", path, "-")
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	if out.over {
		return "", errTooLarge
	}
	return out.String(), nil
}

var blankRuns = regexp.MustCompile(`\n{3,}`)

// cleanText normalizes extracted text: valid UTF-8, no control or invisible
// characters, no trailing spaces, and at most one blank line in a row. What
// comes out is safe to print on a terminal.
func cleanText(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n", "\f", "\n\n").Replace(s)
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n':
			return r
		case unicode.IsSpace(r):
			return ' '
		case !unicode.IsPrint(r):
			return -1
		}
		return r
	}, s)
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimRight(line, " ")
	}
	return strings.TrimSpace(blankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// chunkText cuts text into excerpts of about chunkTarget bytes, breaking at
// paragraph boundaries where it can.
func chunkText(text string) []string {
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if s := strings.TrimSpace(cur.String()); len(s) >= chunkMin {
			chunks = append(chunks, s)
		}
		cur.Reset()
	}
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.TrimSpace(para)
		// A paragraph too long for one excerpt is cut at a line or a word.
		for len(para) > chunkMax {
			flush()
			cut := cutPoint(para, chunkMax)
			cur.WriteString(para[:cut])
			flush()
			para = strings.TrimSpace(para[cut:])
		}
		if para == "" {
			continue
		}
		if cur.Len() > 0 && cur.Len()+2+len(para) > chunkMax {
			flush()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(para)
		if cur.Len() >= chunkTarget {
			flush()
		}
	}
	flush()
	return chunks
}

// cutPoint returns where to cut s so that the first part is at most max
// bytes: after the last line break, else the last space, else between two
// characters.
func cutPoint(s string, max int) int {
	if i := strings.LastIndexByte(s[:max], '\n'); i > max/2 {
		return i
	}
	if i := strings.LastIndexByte(s[:max], ' '); i > max/2 {
		return i
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return cut
}

var (
	// htmlHidden matches what is not the document's own text: code, styles,
	// and the menus and footers a site wraps around every page.
	htmlHidden = regexp.MustCompile(`(?is)<script\b.*?</script\s*>|<style\b.*?</style\s*>|<head\b.*?</head\s*>|<nav\b.*?</nav\s*>|<footer\b.*?</footer\s*>|<!--.*?-->`)
	htmlPre    = regexp.MustCompile(`(?is)<pre\b.*?</pre\s*>`)
	htmlBreak  = regexp.MustCompile(`(?i)<(?:br|/li|/tr|/dt|/dd)\b[^>]*>`)
	htmlBlock  = regexp.MustCompile(`(?i)</?(?:p|div|h[1-6]|pre|section|article|blockquote|table|ul|ol|dl|hr)\b[^>]*>`)
	htmlTag    = regexp.MustCompile(`(?s)<[^>]*>`)
)

// htmlToText strips the markup from an HTML document, keeping paragraph
// breaks.
func htmlToText(s string) string {
	s = htmlHidden.ReplaceAllString(s, " ")
	// Line breaks in the source are only spaces; structure comes from tags.
	// Preformatted text is the exception: there a command on each line has to
	// stay on its line.
	s = htmlPre.ReplaceAllStringFunc(s, func(pre string) string { return strings.ReplaceAll(pre, "\n", "<br>") })
	s = strings.NewReplacer("\r", " ", "\n", " ").Replace(s)
	s = htmlBreak.ReplaceAllString(s, "\n")
	s = htmlBlock.ReplaceAllString(s, "\n\n")
	s = htmlTag.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

var (
	roffFont    = regexp.MustCompile(`\\f(?:\[[^\]]*\]|\(..|.)|\\s[-+]?\d+|\\[hvwl]'[^']*'|\\c`)
	roffSpecial = regexp.MustCompile(`\\\*?(?:\[[^\]]*\]|\(..)`)
	roffSimple  = strings.NewReplacer(`\\`, `\`, `\-`, "-", `\&`, "", `\e`, `\`, `\ `, " ", `\~`, " ",
		`\|`, "", `\^`, "", `\,`, "", `\/`, "", `\(aq`, "'", `\(dq`, `"`, `\(em`, "-", `\(en`, "-",
		`\(bu`, "*", `\(lq`, `"`, `\(rq`, `"`, `\*(lq`, `"`, `\*(rq`, `"`, `\(co`, "(c)")
)

// roffEscapes replaces the roff escape sequences in s with plain characters.
func roffEscapes(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	s = roffFont.ReplaceAllString(s, "")
	s = roffSimple.Replace(s)
	return roffSpecial.ReplaceAllString(s, "")
}

// roffFields splits the arguments of a macro line, keeping double-quoted
// arguments whole and stopping at a trailing comment.
func roffFields(s string) []string {
	var fields []string
	for s = strings.TrimLeft(s, " \t"); s != ""; s = strings.TrimLeft(s, " \t") {
		var f string
		if s[0] == '"' {
			if end := strings.IndexByte(s[1:], '"'); end >= 0 {
				f, s = s[1:1+end], s[end+2:]
			} else {
				f, s = s[1:], ""
			}
		} else {
			if strings.HasPrefix(s, `\"`) {
				break
			}
			if end := strings.IndexAny(s, " \t"); end >= 0 {
				f, s = s[:end], s[end:]
			} else {
				f, s = s, ""
			}
		}
		fields = append(fields, roffEscapes(f))
	}
	return fields
}

// mdocMacros are the mdoc macros that can appear among the arguments of a
// macro line. Most only mark up the words after them.
var mdocMacros = wordSet(`Ac Ad An Ao Ap Aq Ar At Bc Bo Bq Brc Bro Brq Bsx Bx Cd Cm Dc Do Dq Dv Dx
	Ec Em En Eo Er Es Ev Fa Fc Fd Fl Fn Fo Ft Fx Ic In Lb Li Lk Ms Mt Nm No Ns Nx Oc Oo Op Ot Ox
	Pa Pc Pf Po Pq Qc Ql Qo Qq Sc So Sq St Sx Sy Ta Tn Ux Va Vt Xc Xo Xr`)

// mdocInline renders the arguments of an mdoc macro line: ".Op Fl r Ar file"
// becomes "[-r file]" and ".Xr zfs 8 ," becomes "zfs(8),". name is the page's
// own name, which a bare Nm stands for.
func mdocInline(tokens []string, name string) string {
	var out []string
	var closers []string // what the enclosures opened on this line end with
	glue := false        // attach the next word to the previous one

	word := func(s string) {
		if glue && len(out) > 0 {
			out[len(out)-1] += s
		} else {
			out = append(out, s)
		}
		glue = false
	}
	open := func(s string) { word(s); glue = true }
	shut := func(s string) { glue = true; word(s) }
	punct := func(s string) bool { return len(s) == 1 && strings.Contains(".,;:)]?!", s) }
	// operand reports whether tokens[i] exists and is ordinary text.
	operand := func(i int) bool { return i < len(tokens) && !mdocMacros[tokens[i]] && !punct(tokens[i]) }

	for i := 0; i < len(tokens); i++ {
		switch t := tokens[i]; t {
		case "Fl":
			if operand(i + 1) {
				i++
				word("-" + tokens[i])
			} else {
				word("-")
				glue = i+1 < len(tokens) && tokens[i+1] == "Fl" // ".Fl Fl long" is --long
			}
		case "Xr":
			if operand(i + 1) {
				i++
				ref := tokens[i]
				if operand(i + 1) {
					i++
					ref += "(" + tokens[i] + ")"
				}
				word(ref)
			}
		case "Nm":
			if !operand(i+1) && name != "" {
				word(name)
			}
		case "Ns":
			glue = true
		case "Op":
			open("[")
			closers = append(closers, "]")
		case "Oo", "Bo":
			open("[")
		case "Oc", "Bc":
			shut("]")
		case "Bq":
			open("[")
			closers = append(closers, "]")
		case "Pq":
			open("(")
			closers = append(closers, ")")
		case "Po":
			open("(")
		case "Pc":
			shut(")")
		case "Aq":
			open("<")
			closers = append(closers, ">")
		case "Ao":
			open("<")
		case "Ac":
			shut(">")
		case "Dq", "Qq":
			open(`"`)
			closers = append(closers, `"`)
		case "Do", "Qo":
			open(`"`)
		case "Dc", "Qc":
			shut(`"`)
		case "Sq", "Ql":
			open("'")
			closers = append(closers, "'")
		case "So":
			open("'")
		case "Sc":
			shut("'")
		case "Fx":
			word("FreeBSD")
		case "Nx":
			word("NetBSD")
		case "Ox":
			word("OpenBSD")
		case "Bx":
			word("BSD")
		case "Ux":
			word("UNIX")
		default:
			switch {
			case mdocMacros[t]:
			case punct(t):
				shut(t)
			case t == "(" || t == "[":
				open(t)
			default:
				word(t)
			}
		}
	}
	for i := len(closers) - 1; i >= 0; i-- {
		shut(closers[i])
	}
	return strings.Join(out, " ")
}

// manToText renders roff manual page source, written with either the mdoc or
// the man macros, as plain text. It is not a formatter: it keeps the words
// and turns the common macros into the punctuation a reader expects, which is
// enough to search the page and for a language model to read it.
func manToText(src string) string {
	var b strings.Builder
	name := ""
	skipUntil := "" // inside a macro definition or an ignored block: the line that ends it
	for _, line := range strings.Split(src, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if skipUntil != "" {
			if line == skipUntil || line == ".." {
				skipUntil = ""
			}
			continue
		}
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		if !roffControl(line) {
			b.WriteString(roffEscapes(line))
			b.WriteByte('\n')
			continue
		}
		if line[0] == '\\' {
			continue // a comment or a brace on a line of its own
		}
		fields := roffFields(line[1:])
		if len(fields) == 0 {
			continue
		}
		macro, args := fields[0], fields[1:]
		switch macro {
		case "Sh", "Ss", "SH", "SS":
			b.WriteString("\n" + strings.Join(args, " ") + "\n")
		case "Pp", "Lp", "PP", "LP", "P", "TP", "TQ", "HP", "sp", "Sp", "Vb", "Ve", "Bl", "El", "Bd", "Ed", "RS", "RE":
			b.WriteByte('\n')
		case "de", "de1", "am", "ig":
			// The body runs to ".." or, when an end macro is named, to that.
			skipUntil = ".."
			if end := len(args) - 1; end >= 0 && (macro == "ig" || end >= 1) {
				skipUntil = "." + args[end]
			}
		case "IP":
			b.WriteByte('\n')
			if len(args) > 0 {
				b.WriteString(args[0] + "\n")
			}
		case "B", "I", "SM", "SB":
			b.WriteString(strings.Join(args, " ") + "\n")
		case "BR", "BI", "IB", "IR", "RB", "RI":
			b.WriteString(strings.Join(args, "") + "\n")
		case "It":
			b.WriteString("\n" + mdocInline(args, name) + "\n")
		case "Nd":
			b.WriteString("- " + strings.Join(args, " ") + "\n")
		case "Dl", "D1":
			b.WriteString(mdocInline(args, name) + "\n")
		case "Nm":
			if name == "" && len(args) > 0 {
				name = args[0]
			}
			fallthrough
		default:
			// Everything else is either markup for its arguments or, like
			// .Dd and the roff requests, layout and metadata to leave out.
			if mdocMacros[macro] {
				b.WriteString(mdocInline(fields, name) + "\n")
			}
		}
	}
	return b.String()
}
