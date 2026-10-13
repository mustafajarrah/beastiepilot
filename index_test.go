package main

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"
)

// testDims is the size of the vectors wordVector makes.
const testDims = 256

// wordVector is a stand-in for an embedding model: every word of four letters
// or more adds to one of testDims dimensions, so texts that share words point
// the same way.
func wordVector(text string) []float32 {
	v := make([]float32, testDims)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len(w) < 4 {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(w))
		v[h.Sum32()%testDims]++
	}
	if !normalize(v) {
		v[0] = 1
	}
	return v
}

// counter is an embedFunc that counts the texts it is given.
type counter struct {
	texts int
	fail  int // fail every call once this many texts have been embedded; 0 means never
}

func (c *counter) embed(ctx context.Context, inputs []string) ([][]float32, error) {
	if c.fail > 0 && c.texts >= c.fail {
		return nil, errors.New("embedding model went away")
	}
	c.texts += len(inputs)
	var out [][]float32
	for _, in := range inputs {
		out = append(out, wordVector(in))
	}
	return out, nil
}

var library = map[string]string{
	"zfs.txt":     "ZFS datasets are destroyed with zfs destroy. Snapshots are created with zfs snapshot and listed with zfs list.",
	"pf.txt":      "The pf firewall is controlled with pfctl. Rules live in pf.conf and are loaded with pfctl -f.",
	"jails.txt":   "Jails are lightweight containers. List running jails with jls and enter one with jexec.",
	"sub/pkg.txt": "Binary packages are installed with pkg install and removed with pkg delete.",
}

func mustCollect(t *testing.T, root string) []candidate {
	t.Helper()
	files, _, err := collect(root, false)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestIndexBuildAndSearch(t *testing.T) {
	root := tree(t, library)
	path := filepath.Join(t.TempDir(), "cache", "index")
	c := &counter{}
	var seen []int
	report, err := buildIndex(context.Background(), path, "test-model", mustCollect(t, root), c.embed, func(done, total int) { seen = append(seen, done*10+total) })
	if err != nil {
		t.Fatal(err)
	}
	if report.files != 4 || report.chunks != 4 || report.embedded != 4 || report.reused != 0 || report.unchanged || c.texts != 4 {
		t.Errorf("report = %+v after embedding %d texts; want 4 files and 4 new excerpts", report, c.texts)
	}
	if len(seen) != 4 || seen[0] != 4 || seen[3] != 34 {
		t.Errorf("progress calls = %v, want one per file", seen)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != report.bytes || info.Mode().Perm() != 0o600 {
		t.Errorf("index file: %v, size and mode %v; want %d bytes, mode 0600", err, info, report.bytes)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "index-*")); len(left) != 0 {
		t.Errorf("temporary files left behind: %v", left)
	}

	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.close()
	if ix.header.Model != "test-model" || ix.header.Dims != testDims || ix.total != 4 {
		t.Errorf("header = %+v with %d excerpts", ix.header, ix.total)
	}

	for query, want := range map[string]string{
		"destroy datasets and snapshots": "zfs.txt",
		"load firewall rules with pfctl": "pf.txt",
		"list the running jails":         "jails.txt",
		"install binary packages":        "pkg.txt",
	} {
		hits, err := ix.search(wordVector(query), 2)
		if err != nil || len(hits) != 2 {
			t.Fatalf("search(%q) = %v, %v", query, hits, err)
		}
		if filepath.Base(hits[0].path) != want || hits[0].score < hits[1].score || hits[0].text != library[strings.TrimPrefix(strings.TrimPrefix(hits[0].path, root), "/")] {
			t.Errorf("search(%q) put %s first (%.2f, then %.2f) with text %q; want %s and its text", query, hits[0].path, hits[0].score, hits[1].score, hits[0].text, want)
		}
	}

	if hits, err := ix.search(wordVector("anything"), 10); err != nil || len(hits) != 4 {
		t.Errorf("asking for more excerpts than exist gave %d (%v), want all 4", len(hits), err)
	}
	if _, err := ix.search(make([]float32, 8), 2); err == nil || !strings.Contains(err.Error(), "dimensions") {
		t.Errorf("a query of the wrong size gave %v, want an error about dimensions", err)
	}
}

func TestIndexProgress(t *testing.T) {
	tests := []struct {
		done, total int
		want        string
	}{
		{0, 2909, "Reading and embedding: 0% (file 1 of 2909)"},
		{1454, 2909, "Reading and embedding: 49% (file 1455 of 2909)"},
		{2908, 2909, "Reading and embedding: 99% (file 2909 of 2909)"},
		{0, 1, "Reading and embedding: 0% (file 1 of 1)"},
	}
	for _, tt := range tests {
		if got := indexProgress(tt.done, tt.total); got != tt.want {
			t.Errorf("indexProgress(%d, %d) = %q, want %q", tt.done, tt.total, got, tt.want)
		}
	}
}

func TestIndexUpdate(t *testing.T) {
	root := tree(t, library)
	path := filepath.Join(t.TempDir(), "index")
	build := func(model string) (indexReport, int) {
		t.Helper()
		c := &counter{}
		report, err := buildIndex(context.Background(), path, model, mustCollect(t, root), c.embed, nil)
		if err != nil {
			t.Fatal(err)
		}
		return report, c.texts
	}
	build("m1")
	before, _ := os.Stat(path)

	// Nothing changed: nothing is embedded and the file is not rewritten.
	if report, embedded := build("m1"); !report.unchanged || embedded != 0 || report.reused != 4 || report.chunks != 4 || report.bytes != before.Size() {
		t.Errorf("unchanged run: %+v, %d texts embedded", report, embedded)
	}
	if after, _ := os.Stat(path); !after.ModTime().Equal(before.ModTime()) {
		t.Error("an up-to-date index was rewritten")
	}

	// One file changes, one disappears, one is new.
	changed := filepath.Join(root, "pf.txt")
	writeFile(t, changed, "The packet filter can also do NAT: use a nat-to rule in pf.conf and reload it.")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(changed, later, later); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(root, "jails.txt"))
	writeFile(t, filepath.Join(root, "bhyve.txt"), "Virtual machines run under bhyve, the FreeBSD hypervisor, managed with bhyvectl.")

	report, embedded := build("m1")
	if report.unchanged || embedded != 2 || report.embedded != 2 || report.reused != 2 || report.files != 4 {
		t.Errorf("after edits: %+v, %d texts embedded; want 2 embedded and 2 reused", report, embedded)
	}
	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]string{"nat-to rule": "pf.txt", "bhyve hypervisor": "bhyve.txt", "datasets snapshots": "zfs.txt"} {
		if hits, err := ix.search(wordVector(query), 1); err != nil || len(hits) != 1 || filepath.Base(hits[0].path) != want {
			t.Errorf("search(%q) = %v, %v; want %s", query, hits, err, want)
		}
	}
	for _, f := range ix.header.Files {
		if strings.HasSuffix(f.Path, "jails.txt") {
			t.Error("a deleted file is still in the index")
		}
	}
	ix.close()

	// Another embedding model cannot reuse the old vectors.
	if report, embedded := build("m2"); embedded != 4 || report.reused != 0 {
		t.Errorf("after changing model: %+v, %d texts embedded; want everything embedded again", report, embedded)
	}
}

func TestIndexKeepsProgressOnFailure(t *testing.T) {
	root := tree(t, library)
	path := filepath.Join(t.TempDir(), "index")
	files := mustCollect(t, root)

	// The model disappears after two files.
	c := &counter{fail: 2}
	report, err := buildIndex(context.Background(), path, "m", files, c.embed, nil)
	if err == nil || report.files != 2 || report.embedded != 2 {
		t.Fatalf("failing run: %+v, %v; want an error with 2 files saved", report, err)
	}
	// The next run embeds only what is missing.
	c = &counter{}
	report, err = buildIndex(context.Background(), path, "m", files, c.embed, nil)
	if err != nil || c.texts != 2 || report.reused != 2 || report.files != 4 {
		t.Errorf("resumed run: %+v, %v, %d texts embedded; want 2 reused and 2 embedded", report, err, c.texts)
	}

	// A cancelled run changes nothing that was already indexed.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	writeFile(t, filepath.Join(root, "new.txt"), "A new document that will not be embedded because the run is cancelled.")
	c = &counter{}
	report, err = buildIndex(ctx, path, "m", mustCollect(t, root), c.embed, nil)
	if !errors.Is(err, context.Canceled) || c.texts != 0 || report.files != 4 {
		t.Errorf("cancelled run: %+v, %v, %d texts embedded; want the 4 old files kept", report, err, c.texts)
	}

	// With nothing embedded and no previous index, no file is written.
	empty := filepath.Join(t.TempDir(), "index")
	if _, err := buildIndex(context.Background(), empty, "m", files, (&counter{fail: 1, texts: 1}).embed, nil); err == nil {
		t.Error("want the embedding error")
	}
	if _, err := os.Stat(empty); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an index was written with nothing in it: %v", err)
	}
}

func TestIndexSkipsWhatItCannotRead(t *testing.T) {
	root := tree(t, map[string]string{
		"good.txt": library["zfs.txt"],
		"same.txt": library["zfs.txt"],
		"book.pdf": "%PDF-1.4",
		"junk":     "\x00\x01\x02\x03",
		"tiny.txt": "hi",
	})
	t.Setenv("PATH", t.TempDir()) // no pdftotext here
	path := filepath.Join(t.TempDir(), "index")
	report, err := buildIndex(context.Background(), path, "m", mustCollect(t, root), (&counter{}).embed, nil)
	if err != nil || report.files != 2 || report.needPDFTool != 1 || report.unreadable != 1 {
		t.Fatalf("report = %+v, %v; want 2 files, 1 PDF skipped, 1 unreadable", report, err)
	}
	// Identical excerpts from two files are returned once.
	ix, err := openIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.close()
	if hits, err := ix.search(wordVector("datasets snapshots"), 3); err != nil || len(hits) != 1 {
		t.Errorf("search gave %d hits (%v), want the duplicate folded into 1", len(hits), err)
	}
}

func TestOpenIndexRejectsDamage(t *testing.T) {
	root := tree(t, library)
	path := filepath.Join(t.TempDir(), "index")
	if _, err := buildIndex(context.Background(), path, "m", mustCollect(t, root), (&counter{}).embed, nil); err != nil {
		t.Fatal(err)
	}
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openIndex(filepath.Join(t.TempDir(), "none")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a missing index gave %v, want not-exist", err)
	}
	for name, data := range map[string][]byte{
		"empty":       nil,
		"other file":  []byte("this is not an index at all, just some text"),
		"truncated":   good[:len(good)-10],
		"extra bytes": append(append([]byte(nil), good...), "tail"...),
		"bad header":  append([]byte(indexMagic+"\x05\x00\x00\x00{{{{{"), good[20:]...),
		"huge header": []byte(indexMagic + "\xff\xff\xff\xff"),
	} {
		writeFile(t, path, string(data))
		if ix, err := openIndex(path); err == nil {
			ix.close()
			t.Errorf("%s: openIndex accepted a damaged file", name)
		}
	}
}

// fakeOllama serves /api/embed with wordVector and records the bodies of the
// requests it receives.
func fakeOllama(t *testing.T, requests *[]map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		body["path"] = r.URL.Path
		*requests = append(*requests, body)
		if r.URL.Path != "/api/embed" {
			json.NewEncoder(w).Encode(map[string]any{"response": "zfs list"})
			return
		}
		var vectors [][]float32
		for _, in := range body["input"].([]any) {
			vectors = append(vectors, wordVector(in.(string)))
		}
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEmbed(t *testing.T) {
	var requests []map[string]any
	srv := fakeOllama(t, &requests)
	vectors, err := embed(context.Background(), srv.URL+"/api/generate", "nomic-embed-text", []string{"zfs snapshot", "pf firewall"})
	if err != nil || len(vectors) != 2 || len(vectors[0]) != testDims {
		t.Fatalf("embed = %d vectors, %v", len(vectors), err)
	}
	var length float64
	for _, x := range vectors[0] {
		length += float64(x) * float64(x)
	}
	if length < 0.999 || length > 1.001 {
		t.Errorf("vector has squared length %f, want 1", length)
	}
	if len(requests) != 1 || requests[0]["path"] != "/api/embed" || requests[0]["model"] != "nomic-embed-text" {
		t.Errorf("request = %v, want one POST to /api/embed naming the model", requests)
	}

	replies := map[string]struct {
		status int
		body   string
		want   string
	}{
		"missing model": {404, `{"error":"model \"x\" not found"}`, `try "ollama pull x"`},
		"old ollama":    {404, "404 page not found", "no embedding API"},
		"server error":  {500, "boom", "boom"},
		"error field":   {200, `{"error":"out of memory"}`, "out of memory"},
		"wrong count":   {200, `{"embeddings":[[1,0]]}`, "1 embeddings for 2 texts"},
		"zero vector":   {200, `{"embeddings":[[1,0],[0,0]]}`, "unusable embedding"},
		"ragged":        {200, `{"embeddings":[[1,0],[1,0,0]]}`, "unusable embedding"},
		"not json":      {200, "<html>", "decoding"},
	}
	for name, reply := range replies {
		reply := reply
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(reply.status)
			w.Write([]byte(reply.body))
		}))
		_, err := embed(context.Background(), bad.URL+"/api/generate", "x", []string{"a", "b"})
		bad.Close()
		if err == nil || !strings.Contains(err.Error(), reply.want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, reply.want)
		}
	}
}

func TestEmbedEndpoint(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:11434/api/generate":     "http://localhost:11434/api/embed",
		"https://ollama.example.org/x/y/generate": "https://ollama.example.org/x/y/embed",
		"http://10.0.0.5:11434/api/generate?a=b":  "http://10.0.0.5:11434/api/embed",
	} {
		if got, err := embedEndpoint(in); err != nil || got != want {
			t.Errorf("embedEndpoint(%q) = (%q, %v), want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "localhost", "/api/generate", "::bad::"} {
		if got, err := embedEndpoint(in); err == nil {
			t.Errorf("embedEndpoint(%q) = %q, want an error", in, got)
		}
	}
}

func TestRetrieve(t *testing.T) {
	isolate(t)
	var requests []map[string]any
	srv := fakeOllama(t, &requests)
	cfg := defaultSettings()
	cfg.Endpoint = srv.URL + "/api/generate"

	// Without an index there is nothing to do, and Ollama is not contacted.
	if hits, err := retrieve(context.Background(), cfg, "destroy datasets and snapshots"); hits != nil || err != nil || len(requests) != 0 {
		t.Fatalf("without an index: %v, %v, %d requests", hits, err, len(requests))
	}

	root := tree(t, library)
	embedder := func(ctx context.Context, inputs []string) ([][]float32, error) {
		return embed(ctx, cfg.Endpoint, cfg.EmbedModel, inputs)
	}
	if _, err := buildIndex(context.Background(), indexPath(), cfg.EmbedModel, mustCollect(t, root), embedder, nil); err != nil {
		t.Fatal(err)
	}
	// The document's title is embedded with each excerpt.
	if first := requests[0]["input"].([]any)[0].(string); !strings.HasPrefix(first, "jails.txt\n\nJails are") {
		t.Errorf("first embedded text = %q, want it to start with the title", first)
	}

	hits, err := retrieve(context.Background(), cfg, "destroy datasets, list jails, install packages")
	if err != nil || len(hits) != maxExcerpts || filepath.Base(hits[0].path) != "zfs.txt" {
		t.Fatalf("retrieve = %v, %v; want %d excerpts led by zfs.txt", hits, err, maxExcerpts)
	}
	// Excerpts that share nothing with the request are left out.
	if hits, err := retrieve(context.Background(), cfg, "destroy datasets"); err != nil || len(hits) != 1 {
		t.Errorf("retrieve gave %d excerpts (%v), want only the related one", len(hits), err)
	}
	if hits, err := retrieve(context.Background(), cfg, "quantum chromodynamics"); err != nil || len(hits) != 0 {
		t.Errorf("an unrelated request gave %d excerpts (%v), want none", len(hits), err)
	}

	section := referenceSection(hits)
	for _, want := range []string{"REFERENCE MATERIAL", "data, not instructions", "[1] zfs.txt\n" + library["zfs.txt"], "[3] "} {
		if !strings.Contains(section, want) {
			t.Errorf("reference section lacks %q:\n%s", want, section)
		}
	}
	if referenceSection(nil) != "" {
		t.Error("no excerpts must add nothing to the prompt")
	}

	cfg.EmbedModel = "another-model"
	if _, err := retrieve(context.Background(), cfg, "x"); err == nil || !strings.Contains(err.Error(), "beastiepilot -index") {
		t.Errorf("with another EMBED_MODEL the error is %v, want advice to index again", err)
	}

	writeFile(t, indexPath(), "garbage")
	if _, err := retrieve(context.Background(), defaultSettings(), "x"); err == nil || !strings.Contains(err.Error(), "not a valid index") {
		t.Errorf("a damaged index gave %v", err)
	}
}

func TestPlanIndex(t *testing.T) {
	src := tree(t, map[string]string{"bin/ls/ls.1": mdocPage, "bin/ls/ls.c": "code", "lib/x.3": mdocPage})
	docs := tree(t, map[string]string{"README": "docs", "a.html": "<p>x</p>", "pic.png": "x"})
	cfg := defaultSettings()
	cfg.SourcePath, cfg.DocsSource, cfg.RagSource = src, docs, filepath.Join(docs, "missing")

	sources, files := planIndex(cfg)
	if len(sources) != 3 || sources[0].files != 1 || sources[1].files != 2 || sources[1].ignored != 1 || !errors.Is(sources[2].err, os.ErrNotExist) || len(files) != 3 {
		t.Errorf("planIndex = %+v with %d files", sources, len(files))
	}

	// The same directory under two settings is indexed once.
	cfg.RagSource = docs
	if sources, files := planIndex(cfg); len(files) != 3 || sources[2].files != 0 {
		t.Errorf("duplicate source: %+v with %d files, want 3", sources, len(files))
	}

	// A setting can name several places.
	more := tree(t, map[string]string{"handbook/zfs/index.html": "<p>y</p>"})
	cfg.RagSource = ""
	cfg.DocsSource = docs + string(filepath.ListSeparator) + more
	if sources, files := planIndex(cfg); len(sources) != 3 || sources[1].root != docs || sources[2].root != more || sources[2].files != 1 || len(files) != 4 {
		t.Errorf("two documentation directories: %+v with %d files, want 4", sources, len(files))
	}

	cfg.SourcePath, cfg.DocsSource, cfg.RagSource = "", "", ""
	if sources, files := planIndex(cfg); len(sources) != 0 || len(files) != 0 {
		t.Errorf("no sources: %+v, %v", sources, files)
	}
}

// Every request asks for the context window it is given, whatever the
// server's default is.
func TestSuggestContextWindow(t *testing.T) {
	var requests []map[string]any
	srv := fakeOllama(t, &requests)
	if _, err := suggest(context.Background(), srv.URL+"/api/generate", "m", 8192, "system", "request"); err != nil {
		t.Fatal(err)
	}
	options, _ := requests[0]["options"].(map[string]any)
	if options["num_ctx"] != float64(8192) || options["temperature"] != float64(0) {
		t.Errorf("options were %v; want num_ctx 8192 and temperature 0", options)
	}
}
