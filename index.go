package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
)

// The index holds the excerpts of the reference material together with their
// embeddings, so that the excerpts closest in meaning to a request can be
// found. It is a single file:
//
//	magic     8 bytes, indexMagic
//	length    uint32, little-endian: the size of the header
//	header    JSON, an indexHeader
//	vectors   one per excerpt: Dims float32, little-endian, of unit length
//	texts     the excerpts, end to end; the header has their sizes
//
// Excerpts are stored file by file, in the order of the header. Searching
// reads the vectors once from start to end and keeps nothing but the best
// matches, so memory use does not grow with the index.

const indexMagic = "BPINDEX1"

// indexedFile describes one file in the index header.
type indexedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	MTime  int64  `json:"mtime"`  // nanoseconds since the epoch
	Chunks []int  `json:"chunks"` // the size in bytes of each excerpt's text
}

type indexHeader struct {
	Model string        `json:"model"` // the embedding model the vectors came from
	Dims  int           `json:"dims"`  // the length of every vector
	Files []indexedFile `json:"files"`
}

// indexPath returns where the index is kept: under the user's cache
// directory, since it can always be rebuilt. It is "" if that is unknown.
func indexPath() string {
	dir := os.Getenv("XDG_CACHE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".cache")
	}
	return filepath.Join(dir, "beastiepilot", "index")
}

// index is an index file opened for reading.
type index struct {
	f      *os.File
	header indexHeader
	total  int     // number of excerpts
	first  []int   // for each file, the ordinal of its first excerpt
	textAt []int64 // for each file, where its first excerpt starts in the texts
	vecOff int64   // where the vectors start in the file
	txtOff int64   // where the texts start in the file
}

// openIndex opens the index at path and checks that it is intact. The error
// satisfies errors.Is(err, os.ErrNotExist) when there is no index.
func openIndex(path string) (*index, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	ix, err := readIndex(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return ix, nil
}

func readIndex(f *os.File) (*index, error) {
	corrupt := errors.New("not a valid index (remove it and run beastiepilot -index)")

	prefix := make([]byte, len(indexMagic)+4)
	if _, err := io.ReadFull(f, prefix); err != nil || string(prefix[:len(indexMagic)]) != indexMagic {
		return nil, corrupt
	}
	headerLen := binary.LittleEndian.Uint32(prefix[len(indexMagic):])
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if int64(headerLen) > info.Size() {
		return nil, corrupt
	}
	raw := make([]byte, headerLen)
	if _, err := io.ReadFull(f, raw); err != nil {
		return nil, corrupt
	}
	ix := &index{f: f}
	if err := json.Unmarshal(raw, &ix.header); err != nil || ix.header.Dims <= 0 || ix.header.Dims > 1<<16 {
		return nil, corrupt
	}

	var textBytes int64
	for _, file := range ix.header.Files {
		ix.first = append(ix.first, ix.total)
		ix.textAt = append(ix.textAt, textBytes)
		for _, size := range file.Chunks {
			if size < 0 {
				return nil, corrupt
			}
			textBytes += int64(size)
		}
		ix.total += len(file.Chunks)
	}
	ix.vecOff = int64(len(prefix)) + int64(headerLen)
	ix.txtOff = ix.vecOff + int64(ix.total)*int64(ix.header.Dims)*4
	if info.Size() != ix.txtOff+textBytes {
		return nil, corrupt
	}
	return ix, nil
}

func (ix *index) close() { ix.f.Close() }

// hit is an excerpt found by a search.
type hit struct {
	path  string
	text  string
	score float32 // cosine similarity to the query, from -1 to 1
}

// decodeVector reads a stored vector from raw into v.
func decodeVector(raw []byte, v []float32) {
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
}

// search returns the k excerpts whose vectors are closest to query, best
// first. query must be of unit length, like the stored vectors, so that the
// dot product is the cosine similarity. Excerpts with identical text, which
// happen when the same document is indexed from two places, are returned once.
func (ix *index) search(query []float32, k int) ([]hit, error) {
	dims := ix.header.Dims
	if len(query) != dims {
		return nil, fmt.Errorf("the index holds vectors of %d dimensions but the query has %d; run beastiepilot -index", dims, len(query))
	}

	type scored struct {
		ordinal int
		score   float32
	}
	// Keep twice as many candidates as wanted, to have spares for duplicates.
	keep := 2 * k
	var best []scored // sorted, best first

	r := bufio.NewReaderSize(io.NewSectionReader(ix.f, ix.vecOff, ix.txtOff-ix.vecOff), 1<<20)
	raw := make([]byte, dims*4)
	for n := 0; n < ix.total; n++ {
		if _, err := io.ReadFull(r, raw); err != nil {
			return nil, fmt.Errorf("reading the index: %w", err)
		}
		var dot float32
		for i, q := range query {
			dot += q * math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
		}
		if len(best) == keep && dot <= best[keep-1].score {
			continue
		}
		at := sort.Search(len(best), func(i int) bool { return best[i].score < dot })
		best = append(best, scored{})
		copy(best[at+1:], best[at:])
		best[at] = scored{n, dot}
		if len(best) > keep {
			best = best[:keep]
		}
	}

	var hits []hit
	seen := make(map[string]bool)
	for _, b := range best {
		if len(hits) == k {
			break
		}
		// The file holding excerpt b.ordinal is the last one that starts at or
		// before it.
		file := sort.Search(len(ix.first), func(i int) bool { return ix.first[i] > b.ordinal }) - 1
		entry := ix.header.Files[file]
		offset := ix.txtOff + ix.textAt[file]
		chunk := b.ordinal - ix.first[file]
		for _, size := range entry.Chunks[:chunk] {
			offset += int64(size)
		}
		text := make([]byte, entry.Chunks[chunk])
		if _, err := ix.f.ReadAt(text, offset); err != nil {
			return nil, fmt.Errorf("reading the index: %w", err)
		}
		if seen[string(text)] {
			continue
		}
		seen[string(text)] = true
		hits = append(hits, hit{entry.Path, string(text), b.score})
	}
	return hits, nil
}

// load returns the excerpts and raw vectors of the file at position n in the
// header, for carrying them over to a new index.
func (ix *index) load(n int) (texts []string, vectors []byte, err error) {
	entry := ix.header.Files[n]
	vectors = make([]byte, len(entry.Chunks)*ix.header.Dims*4)
	if _, err := ix.f.ReadAt(vectors, ix.vecOff+int64(ix.first[n])*int64(ix.header.Dims)*4); err != nil {
		return nil, nil, err
	}
	offset := ix.txtOff + ix.textAt[n]
	for _, size := range entry.Chunks {
		text := make([]byte, size)
		if _, err := ix.f.ReadAt(text, offset); err != nil {
			return nil, nil, err
		}
		texts = append(texts, string(text))
		offset += int64(size)
	}
	return texts, vectors, nil
}

// indexWriter assembles a new index. Vectors and texts go to temporary files
// as they arrive, so building a large index does not need it all in memory.
type indexWriter struct {
	header   indexHeader
	dir      string
	vec, txt *os.File
	vecW     *bufio.Writer
	txtW     *bufio.Writer
	chunks   int
}

func newIndexWriter(dir, model string) (*indexWriter, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	w := &indexWriter{header: indexHeader{Model: model}, dir: dir}
	var err error
	if w.vec, err = os.CreateTemp(dir, "index-vectors-*"); err != nil {
		return nil, err
	}
	if w.txt, err = os.CreateTemp(dir, "index-texts-*"); err != nil {
		w.discard()
		return nil, err
	}
	w.vecW, w.txtW = bufio.NewWriterSize(w.vec, 1<<20), bufio.NewWriterSize(w.txt, 1<<20)
	return w, nil
}

// discard removes the temporary files.
func (w *indexWriter) discard() {
	for _, f := range []*os.File{w.vec, w.txt} {
		if f != nil {
			f.Close()
			os.Remove(f.Name())
		}
	}
}

// add appends a file with its excerpts and their vectors, given in the
// stored encoding: len(texts) vectors of dims float32 each.
func (w *indexWriter) add(c candidate, texts []string, vectors []byte, dims int) error {
	switch {
	case len(texts) == 0 || len(vectors) != len(texts)*dims*4:
		return errors.New("an excerpt is missing its vector")
	case w.header.Dims == 0:
		w.header.Dims = dims
	case w.header.Dims != dims:
		return fmt.Errorf("the embedding model returned vectors of %d dimensions where the index has %d; remove the index and build it again", dims, w.header.Dims)
	}
	entry := indexedFile{Path: c.path, Size: c.size, MTime: c.mtime}
	for _, text := range texts {
		entry.Chunks = append(entry.Chunks, len(text))
		if _, err := w.txtW.WriteString(text); err != nil {
			return err
		}
	}
	if _, err := w.vecW.Write(vectors); err != nil {
		return err
	}
	w.header.Files = append(w.header.Files, entry)
	w.chunks += len(texts)
	return nil
}

// finish writes the index to path, replacing any index already there only
// once the new one is complete. It returns the size of the index.
func (w *indexWriter) finish(path string) (int64, error) {
	defer w.discard()
	if w.header.Dims == 0 {
		w.header.Dims = 1 // an index with no excerpts still needs a valid header
	}
	header, err := json.Marshal(w.header)
	if err != nil {
		return 0, err
	}
	out, err := os.CreateTemp(w.dir, "index-new-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(out.Name()) // a no-op once it has been renamed

	write := func() error {
		bw := bufio.NewWriterSize(out, 1<<20)
		bw.WriteString(indexMagic)
		binary.Write(bw, binary.LittleEndian, uint32(len(header)))
		bw.Write(header)
		for _, part := range []struct {
			f *os.File
			w *bufio.Writer
		}{{w.vec, w.vecW}, {w.txt, w.txtW}} {
			if err := part.w.Flush(); err != nil {
				return err
			}
			if _, err := part.f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if _, err := io.Copy(bw, part.f); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	if err := write(); err != nil {
		out.Close()
		return 0, err
	}
	info, err := out.Stat()
	if err != nil {
		out.Close()
		return 0, err
	}
	if err := out.Close(); err != nil {
		return 0, err
	}
	return info.Size(), os.Rename(out.Name(), path)
}

// upToDate reports whether the index at path already holds exactly files, as
// embedded by model, so that building it again would change nothing.
func upToDate(path, model string, files []candidate) bool {
	ix, err := openIndex(path)
	if err != nil {
		return false
	}
	defer ix.close()
	if ix.header.Model != model || len(ix.header.Files) != len(files) {
		return false
	}
	have := make(map[string]indexedFile, len(ix.header.Files))
	for _, f := range ix.header.Files {
		have[f.Path] = f
	}
	for _, c := range files {
		if f, ok := have[c.path]; !ok || f.Size != c.size || f.MTime != c.mtime {
			return false
		}
	}
	return true
}

// embedFunc returns one unit-length vector for each input text.
type embedFunc func(ctx context.Context, inputs []string) ([][]float32, error)

// embedBatch is how many excerpts are sent to the embedding model at a time.
const embedBatch = 32

// encodeVectors puts vectors in the stored encoding.
func encodeVectors(vectors [][]float32) []byte {
	if len(vectors) == 0 {
		return nil
	}
	raw := make([]byte, 0, len(vectors)*len(vectors[0])*4)
	for _, v := range vectors {
		for _, x := range v {
			raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(x))
		}
	}
	return raw
}

// indexReport says what building the index did.
type indexReport struct {
	files, chunks int   // what the index now holds
	embedded      int   // excerpts embedded during this run
	reused        int   // files carried over unchanged from the previous index
	unreadable    int   // files that could not be read as text
	needPDFTool   int   // PDF files skipped because pdftotext is not installed
	bytes         int64 // size of the index
	unchanged     bool  // the previous index was already up to date
}

// buildIndex brings the index at path up to date with files, embedding their
// excerpts with embed. Files that have not changed since the previous index
// was built, judged by size and modification time, keep their vectors, which
// makes updating cheap and lets an interrupted run be resumed.
//
// If embedding fails or ctx is cancelled, what has been embedded so far is
// still saved and the error is returned along with the report. progress, if
// not nil, is called before each file with the number already handled.
func buildIndex(ctx context.Context, path, model string, files []candidate, embed embedFunc, progress func(done, total int)) (indexReport, error) {
	var report indexReport

	// The previous index is only useful if it was made by the same model.
	var old *index
	oldFiles := make(map[string]int)
	if ix, err := openIndex(path); err == nil {
		defer ix.close()
		if ix.header.Model == model {
			old = ix
			for n, f := range ix.header.Files {
				oldFiles[f.Path] = n
			}
		}
	}

	w, err := newIndexWriter(filepath.Dir(path), model)
	if err != nil {
		return report, err
	}
	defer w.discard()

	x := newExtractor()
	var failure error
	for n, c := range files {
		if progress != nil {
			progress(n, len(files))
		}
		if o, ok := oldFiles[c.path]; ok && old.header.Files[o].Size == c.size && old.header.Files[o].MTime == c.mtime {
			if texts, vectors, err := old.load(o); err == nil {
				if err := w.add(c, texts, vectors, old.header.Dims); err != nil {
					return report, err
				}
				report.reused++
				continue
			}
		}
		if failure != nil {
			continue // keep carrying over what is already embedded
		}
		if failure = ctx.Err(); failure != nil {
			continue
		}

		text, err := x.extract(ctx, c)
		if errors.Is(err, errNoPDFTool) {
			report.needPDFTool++
			continue
		}
		if err != nil {
			report.unreadable++
			continue
		}
		chunks := chunkText(text)
		if len(chunks) == 0 {
			continue
		}
		var vectors [][]float32
		title := docTitle(c.path)
		for start := 0; start < len(chunks) && failure == nil; start += embedBatch {
			batch := chunks[start:min(start+embedBatch, len(chunks))]
			inputs := make([]string, len(batch))
			for i, chunk := range batch {
				// The title tells the model what the excerpt is about; an
				// option's description rarely repeats the command's name.
				inputs[i] = title + "\n\n" + chunk
			}
			var got [][]float32
			if got, failure = embed(ctx, inputs); failure == nil {
				vectors = append(vectors, got...)
			}
		}
		if failure != nil {
			continue
		}
		if err := w.add(c, chunks, encodeVectors(vectors), len(vectors[0])); err != nil {
			failure = err
			continue
		}
		report.embedded += len(chunks)
	}

	report.files, report.chunks = len(w.header.Files), w.chunks
	if failure == nil && old != nil && report.embedded == 0 && report.reused == len(old.header.Files) {
		// Nothing was added, changed or removed.
		report.unchanged = true
		if info, err := old.f.Stat(); err == nil {
			report.bytes = info.Size()
		}
		return report, nil
	}
	if w.chunks == 0 {
		// Nothing to store. An existing index is left alone rather than
		// replaced by an empty one because of, say, an unmounted disk.
		return report, failure
	}
	if report.bytes, err = w.finish(path); err != nil {
		return report, err
	}
	return report, failure
}
