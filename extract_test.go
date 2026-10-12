package main

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		kind docKind
		gz   bool
	}{
		{"notes.txt", kindText, false},
		{"Guide.MD", kindText, false},
		{"README", kindText, false},
		{"paper.ascii.gz", kindText, true},
		{"zfs.8", kindMan, false},
		{"zfs.8.gz", kindMan, true},
		{"rc.conf.5", kindMan, false},
		{"index.html", kindHTML, false},
		{"book.epub", kindEPUB, false},
		{"Book.PDF", kindPDF, false},
		{"book.pdf.gz", kindNone, false},
		{"main.c", kindNone, false},
		{"photo.jpg", kindNone, false},
		{"archive.tar.gz", kindNone, false},
		{".hidden.txt", kindNone, false},
		{"lib.so.10", kindNone, false},
	}
	for _, tt := range tests {
		if kind, gz := classify("/some/dir/" + tt.name); kind != tt.kind || gz != tt.gz {
			t.Errorf("classify(%q) = (%d, %v), want (%d, %v)", tt.name, kind, gz, tt.kind, tt.gz)
		}
	}
}

func TestDocTitle(t *testing.T) {
	for path, want := range map[string]string{
		"/usr/src/sbin/mount/mount.8":          "mount(8)",
		"/usr/share/man/man5/rc.conf.5.gz":     "rc.conf(5)",
		"/home/me/books/Absolute FreeBSD.epub": "Absolute FreeBSD.epub",
		"/usr/share/doc/README":                "README",
		"/x/notes.txt.gz":                      "notes.txt",
	} {
		if got := docTitle(path); got != want {
			t.Errorf("docTitle(%q) = %q, want %q", path, got, want)
		}
	}
}

const mdocPage = `.\" Copyright (c) 2024 Someone
.\" A long licence comment.
.\"
.Dd June 1, 2024
.Dt ZFS-DESTROY 8
.Os
.Sh NAME
.Nm zfs-destroy
.Nd destroy ZFS dataset, snapshots, or bookmark
.Sh SYNOPSIS
.Nm zfs
.Cm destroy
.Op Fl Rfnprv
.Ar filesystem Ns | Ns Ar volume
.Sh DESCRIPTION
Destroys the given dataset.
By default, the command unshares any file systems that are currently shared.
.Bl -tag -width "-R"
.It Fl R
Recursively destroy all dependents, including cloned file systems.
.It Fl r , Fl Fl recursive
Recursively destroy all children.
.El
.Pp
See also
.Xr zfs-list 8 ,
.Xr zpool 8 .
Use
.Dq Li zfs list
and the
.Pa /etc/rc.conf
file \(em carefully\&.
`

func TestManToText(t *testing.T) {
	got := cleanText(manToText(mdocPage))
	for _, want := range []string{
		"NAME\nzfs-destroy\n- destroy ZFS dataset, snapshots, or bookmark",
		"SYNOPSIS\nzfs\ndestroy\n[-Rfnprv]\nfilesystem|volume",
		"DESCRIPTION\nDestroys the given dataset.",
		"\n\n-R\nRecursively destroy all dependents",
		"\n\n-r, --recursive\nRecursively destroy all children.",
		"zfs-list(8),\nzpool(8).",
		"\"zfs list\"",
		"/etc/rc.conf\nfile - carefully.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("manToText output lacks %q; got:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"Copyright", "June 1", ".Sh", "Fl", "-tag", `\(em`, `\&`} {
		if strings.Contains(got, unwanted) {
			t.Errorf("manToText output still contains %q; got:\n%s", unwanted, got)
		}
	}
}

func TestManToTextOldMacros(t *testing.T) {
	src := `.de Sp \" Vertical space
.if t .sp .5v
text inside a definition
..
.ig
ignored words
..
\{\
.TH RSYNC 1 "2024" "rsync"
.SH NAME
rsync \- a fast, versatile file-copying tool
.SH OPTIONS
.TP
.B \-a, \-\-archive
archive mode; \fBsame\fP as \-rlptgoD on \s-1UNIX\s0
.IP "\-v" 4
increase verbosity
.BR ssh (1)
`
	got := cleanText(manToText(src))
	for _, want := range []string{"NAME\nrsync - a fast, versatile file-copying tool", "-a, --archive\narchive mode; same as -rlptgoD on UNIX", "-v\nincrease verbosity", "ssh(1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "definition") || strings.Contains(got, "ignored") || strings.Contains(got, "{") {
		t.Errorf("macro definitions, ignored blocks and braces should be left out; got:\n%s", got)
	}
}

func TestLooksLikeManual(t *testing.T) {
	for text, want := range map[string]bool{
		mdocPage:                                true,
		".TH FOO 1\n":                           true,
		"'\\\" t\n.\\\" comment\n\n.Dd today\n": true,
		// Generated pages open with macro definitions, whose bodies hold text.
		".de Sh\n\\fB\\\\$1\\fR\n..\n\\{\\\n\\\"odd comment\n.TH X 1\n": true,
		".hidden\nnot roff\n.TH X 1\n":                                  false,
		"#include <stdio.h>\n":                                          false,
		"just some words\n.Sh NAME\n":                                   false,
		"":                                                              false,
		".\\\" only comments\n":                                         false,
	} {
		if got := looksLikeManual(text); got != want {
			t.Errorf("looksLikeManual(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestHTMLToText(t *testing.T) {
	src := `<html><head><title>Ignored</title><style>p { color: red }</style></head>
<body><!-- a comment --><h1>Jails</h1>
<p>A jail is a <b>lightweight</b>
container &amp; more.</p><script>alert("x")</script>
<ul><li>jls&nbsp;lists them</li><li>jexec enters one</li></ul></body></html>`
	got := cleanText(htmlToText(src))
	want := "Jails\n\nA jail is a lightweight container & more.\n\njls lists them\njexec enters one"
	if got != want {
		t.Errorf("htmlToText gave:\n%q\nwant:\n%q", got, want)
	}
}

func TestCleanText(t *testing.T) {
	got := cleanText("a\x1b[31mb\r\nline two  \t\n\n\n\n\x00third​ word here\fnext page\xff")
	want := "a[31mb\nline two\n\nthird word here\n\nnext page"
	if got != want {
		t.Errorf("cleanText gave %q, want %q", got, want)
	}
}

func TestChunkText(t *testing.T) {
	para := strings.Repeat("word ", 60) // 300 bytes
	var paras []string
	for i := 0; i < 10; i++ {
		paras = append(paras, strings.TrimSpace(para))
	}
	long := strings.TrimSpace(strings.Repeat("alpha beta gamma ", 300)) // 5100 bytes, one paragraph
	text := strings.Join(paras, "\n\n") + "\n\n" + long + "\n\ntiny\n\n" + strings.Repeat("é", 2000)

	chunks := chunkText(text)
	if len(chunks) < 6 {
		t.Fatalf("got %d chunks, want the text split into several", len(chunks))
	}
	var words int
	for i, c := range chunks {
		if len(c) > chunkMax || len(c) < chunkMin {
			t.Errorf("chunk %d is %d bytes, want between %d and %d", i, len(c), chunkMin, chunkMax)
		}
		if c != strings.TrimSpace(c) || !strings.Contains(c, "") || strings.ToValidUTF8(c, "") != c {
			t.Errorf("chunk %d is not trimmed, valid text: %q", i, c)
		}
		words += len(strings.Fields(c))
	}
	// Nothing but the scrap too small to keep is lost.
	if want := len(strings.Fields(text)) - 1; words < want {
		t.Errorf("chunks hold %d words, want at least %d", words, want)
	}
	if got := chunkText("short"); got != nil {
		t.Errorf("a scrap gave %q, want no chunks", got)
	}
	if got := chunkText(strings.Repeat("x", 100)); len(got) != 1 {
		t.Errorf("a small text gave %d chunks, want 1", len(got))
	}
}

// tree creates files under a new temporary directory and returns it with
// symbolic links resolved, as collect reports paths.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		writeFile(t, filepath.Join(root, name), content)
	}
	return root
}

func names(root string, files []candidate) string {
	var out []string
	for _, c := range files {
		rel, _ := filepath.Rel(root, c.path)
		out = append(out, rel)
	}
	return strings.Join(out, " ")
}

func TestCollect(t *testing.T) {
	root := tree(t, map[string]string{
		"sbin/mount/mount.8":       mdocPage,
		"sbin/mount/mount.c":       "int main(void) { return 0; }",
		"lib/libc/sys/open.2":      mdocPage,
		"lib/libc/string/strlen.3": mdocPage,
		"share/man/man5/rc.conf.5": mdocPage,
		"share/man/man4/em.4":      mdocPage,
		"share/man/man9/malloc.9":  mdocPage,
		"README.md":                "# readme",
		".git/config.txt":          "hidden",
		"notes/.secret.txt":        "hidden",
		"photo.jpg":                "\xff\xd8",
	})
	if err := os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}

	files, ignored, err := collect(root, true)
	if got, want := names(root, files), "sbin/mount/mount.8 share/man/man4/em.4 share/man/man5/rc.conf.5"; err != nil || got != want || ignored != 0 {
		t.Errorf("collect(manualOnly) = (%q, %d, %v), want %q", got, ignored, err, want)
	}

	files, ignored, err = collect(root, false)
	want := "README.md lib/libc/string/strlen.3 lib/libc/sys/open.2 sbin/mount/mount.8 share/man/man4/em.4 share/man/man5/rc.conf.5 share/man/man9/malloc.9"
	if got := names(root, files); err != nil || got != want || ignored != 3 {
		t.Errorf("collect = (%q, %d, %v), want %q and 3 ignored", got, ignored, err, want)
	}

	// A single file, and a symbolic link to a directory, both work as roots.
	if files, _, err := collect(filepath.Join(root, "README.md"), false); err != nil || len(files) != 1 {
		t.Errorf("collect(file) = (%v, %v), want that one file", files, err)
	}
	link := filepath.Join(t.TempDir(), "src")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if files, _, err := collect(link, true); err != nil || len(files) != 3 {
		t.Errorf("collect(symlink) found %d files (%v), want 3", len(files), err)
	}
	if _, _, err := collect(filepath.Join(root, "missing"), false); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("collect(missing) error = %v, want not-exist", err)
	}
}

func gzipped(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	zw.Write([]byte(s))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExtract(t *testing.T) {
	var epub bytes.Buffer
	zw := zip.NewWriter(&epub)
	for name, content := range map[string]string{
		"mimetype":           "application/epub+zip",
		"OEBPS/ch1.xhtml":    "<html><body><h1>Boot environments</h1><p>bectl manages them.</p></body></html>",
		"OEBPS/style.css":    "p { margin: 0 }",
		"OEBPS/images/a.png": "\x89PNG",
	} {
		f, _ := zw.Create(name)
		f.Write([]byte(content))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	root := tree(t, map[string]string{
		"plain.txt":     "Plain  text\twith tabs.\n",
		"page.8.gz":     gzipped(t, mdocPage),
		"doc.html":      "<p>Hello <i>there</i></p>",
		"book.epub":     epub.String(),
		"binary":        "\x7fELF\x00\x00\x00binary",
		"libfoo.so.7":   "\x7fELF\x00\x00",
		"data.5":        "1,2,3\n4,5,6\n",
		"broken.txt.gz": "not gzip at all",
	})
	x := extractor{}
	get := func(name string) (string, error) {
		path := filepath.Join(root, name)
		kind, gz := classify(path)
		return x.extract(context.Background(), candidate{path: path, kind: kind, gz: gz})
	}

	for name, want := range map[string]string{
		"plain.txt": "Plain  text with tabs.",
		"doc.html":  "Hello there",
		"book.epub": "Boot environments\n\nbectl manages them.",
	} {
		if got, err := get(name); err != nil || got != want {
			t.Errorf("extract(%s) = (%q, %v), want %q", name, got, err, want)
		}
	}
	if got, err := get("page.8.gz"); err != nil || !strings.Contains(got, "Recursively destroy all children.") {
		t.Errorf("extract(page.8.gz) = (%q, %v), want the page's text", got, err)
	}
	for _, name := range []string{"binary", "libfoo.so.7", "data.5", "broken.txt.gz"} {
		if got, err := get(name); err == nil {
			t.Errorf("extract(%s) = %q, want an error", name, got)
		}
	}
}

func TestExtractPDF(t *testing.T) {
	root := tree(t, map[string]string{"book.pdf": "%PDF-1.4 pretend"})
	c := candidate{path: filepath.Join(root, "book.pdf"), kind: kindPDF}

	if _, err := (extractor{}).extract(context.Background(), c); !errors.Is(err, errNoPDFTool) {
		t.Errorf("without pdftotext the error is %v, want errNoPDFTool", err)
	}

	// A stand-in for pdftotext that reports how it was called.
	tool := filepath.Join(root, "pdftotext")
	script := "#!/bin/sh\nprintf 'Page one of %s\\fPage two (%s)\\n' \"$4\" \"$1 $2 $3 $5\"\n"
	if err := os.WriteFile(tool, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := extractor{pdftotext: tool}.extract(context.Background(), c)
	want := "Page one of " + c.path + "\n\nPage two (-q -enc UTF-8 -)"
	if err != nil || got != want {
		t.Errorf("extract(pdf) = (%q, %v), want %q", got, err, want)
	}

	failing := filepath.Join(root, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (extractor{pdftotext: failing}).extract(context.Background(), c); err == nil {
		t.Error("a failing pdftotext should be an error")
	}
}
