package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// errRenameDiff marca un diff con metadatos de rename. El caller no trocea el
// archivo: el troceo solo es seguro cuando P y C difieren únicamente en hunks
// del mismo path.
var errRenameDiff = errors.New("diff con metadatos de rename")

// noNewlineMarker es la línea que git emite tras el último renglón de un
// archivo sin salto final.
const noNewlineMarker = `\ No newline at end of file`

// diffHunk es un hunk de `git diff -U0`. Lines conserva el prefijo de cada
// renglón (' ', '+' o '-') y los marcadores de fin de archivo.
type diffHunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []string
}

// chunk es un par de commits sintéticos que aísla un grupo de hunks. Parent es
// la cabeza con el trozo revertido y Commit tiene exactamente el árbol de head.
// Los SHA son internos: no se muestran al usuario ni se registran en cobertura.
type chunk struct {
	Parent string
	Commit string
	Hunks  []diffHunk
}

// gitRaw ejecuta git y devuelve stdout sin recortar, para blobs que deben
// conservar CRLF y el salto final ausente. op es una etiqueta fija: los
// argumentos no se incluyen en el error para no filtrar el token del remote.
func gitRaw(ctx context.Context, dir, op string, args ...string) ([]byte, error) {
	return gitRawEnv(ctx, dir, op, nil, nil, args...)
}

// gitRawEnv es gitRaw con entorno adicional y stdin opcional.
func gitRawEnv(ctx context.Context, dir, op string, extraEnv []string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	if len(extraEnv) > 0 {
		cmd.Env = append(os.Environ(), extraEnv...)
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, fmt.Errorf("git %s: %w", op, err)
		}
		return nil, fmt.Errorf("git %s: %w: %s", op, err, detail)
	}
	return stdout.Bytes(), nil
}

// parseFileDiff interpreta la salida de `git diff -U0 --no-color --no-renames`
// para un único path. Los diffs binarios no tienen hunks y devuelven nil.
func parseFileDiff(raw []byte) ([]diffHunk, error) {
	lines := strings.Split(string(raw), "\n")
	var hunks []diffHunk
	var current *diffHunk

	flush := func() {
		if current != nil {
			hunks = append(hunks, *current)
			current = nil
		}
	}

	for _, line := range lines {
		if current != nil && !strings.HasPrefix(line, "@@ ") {
			if line == "" {
				// Artefacto del salto de línea final de la salida.
				continue
			}
			switch line[0] {
			case ' ', '+', '-', '\\':
				current.Lines = append(current.Lines, line)
				continue
			default:
				return nil, fmt.Errorf("diff: cuerpo inesperado %q", line)
			}
		}

		if isRenameMetadata(line) {
			return nil, errRenameDiff
		}
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}

		flush()
		hunk, err := parseHunkHeader(line)
		if err != nil {
			return nil, err
		}
		current = &hunk
	}
	flush()

	return hunks, nil
}

func isRenameMetadata(line string) bool {
	for _, prefix := range []string{
		"similarity index ",
		"dissimilarity index ",
		"rename from ",
		"rename to ",
		"copy from ",
		"copy to ",
	} {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func parseHunkHeader(line string) (diffHunk, error) {
	fields := strings.Fields(line)
	if len(fields) < 4 || fields[0] != "@@" || fields[3] != "@@" {
		return diffHunk{}, fmt.Errorf("diff: cabecera de hunk inválida %q", line)
	}
	oldStart, oldCount, err := parseDiffRange(fields[1], '-')
	if err != nil {
		return diffHunk{}, err
	}
	newStart, newCount, err := parseDiffRange(fields[2], '+')
	if err != nil {
		return diffHunk{}, err
	}
	return diffHunk{
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}, nil
}

func parseDiffRange(field string, sign byte) (start, count int, err error) {
	if len(field) < 2 || field[0] != sign {
		return 0, 0, fmt.Errorf("diff: rango inválido %q", field)
	}
	parts := strings.SplitN(field[1:], ",", 2)
	start, err = strconv.Atoi(parts[0])
	if err != nil || start < 0 {
		return 0, 0, fmt.Errorf("diff: rango inválido %q", field)
	}
	count = 1
	if len(parts) == 2 {
		count, err = strconv.Atoi(parts[1])
		if err != nil || count < 0 {
			return 0, 0, fmt.Errorf("diff: rango inválido %q", field)
		}
	}
	return start, count, nil
}

// hunkSize es el tamaño serializado del hunk: su cabecera y cada renglón con su
// salto. Las cabeceras son pequeñas, pero contarlas evita que el trozo supere
// el presupuesto por acumulación.
func hunkSize(h diffHunk) int {
	size := len(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", h.OldStart, h.OldCount, h.NewStart, h.NewCount))
	for _, line := range h.Lines {
		size += len(line) + 1
	}
	return size
}

// minWindowBytes is the smallest diff window worth its own OCR conversation.
// Below it the header and a single line dominate, so splitting an oversized
// hunk would multiply calls without reducing any prompt meaningfully.
const minWindowBytes = 2 << 10

// windowHeaderReserve covers the serialized `@@ -a,b +c,d @@` header when a
// window is measured before its final positions are known.
const windowHeaderReserve = 48

// windowHunks splits every hunk larger than maxBytes into consecutive windows
// that never share a line, so an unchanged context is not re-sent. When the
// budget is too small for a useful window the hunks stay whole and groupHunks
// still gives each oversized hunk its own group.
func windowHunks(hunks []diffHunk, maxBytes int) []diffHunk {
	if maxBytes < minWindowBytes {
		return hunks
	}
	windows := make([]diffHunk, 0, len(hunks))
	for _, hunk := range hunks {
		windows = append(windows, splitHunk(hunk, maxBytes)...)
	}
	return windows
}

// splitHunk cuts one hunk at line boundaries. Each piece keeps the old and new
// positions of the lines it contains, so reverting it from head reproduces
// exactly that window. A no-newline marker stays with the line it annotates.
func splitHunk(h diffHunk, maxBytes int) []diffHunk {
	if hunkSize(h) <= maxBytes {
		return []diffHunk{h}
	}

	var windows []diffHunk
	var lines []string
	oldCount, newCount := 0, 0
	startOld, startNew := 0, 0
	consumedOld, consumedNew := 0, 0
	size := windowHeaderReserve

	flush := func() {
		if len(lines) == 0 {
			return
		}
		windows = append(windows, diffHunk{
			OldStart: windowStart(h.OldStart, h.OldCount, startOld, oldCount),
			OldCount: oldCount,
			NewStart: windowStart(h.NewStart, h.NewCount, startNew, newCount),
			NewCount: newCount,
			Lines:    append([]string(nil), lines...),
		})
		lines = nil
		oldCount, newCount = 0, 0
		size = windowHeaderReserve
	}

	for _, line := range h.Lines {
		if strings.HasPrefix(line, `\`) {
			// The marker annotates the previous line and must travel with it.
			lines = append(lines, line)
			size += len(line) + 1
			continue
		}
		if len(lines) > 0 && size+len(line)+1 > maxBytes {
			flush()
		}
		if len(lines) == 0 {
			startOld, startNew = consumedOld, consumedNew
		}
		lines = append(lines, line)
		size += len(line) + 1
		if line == "" {
			continue
		}
		switch line[0] {
		case ' ':
			oldCount++
			newCount++
			consumedOld++
			consumedNew++
		case '-':
			oldCount++
			consumedOld++
		case '+':
			newCount++
			consumedNew++
		}
	}
	flush()
	return windows
}

// windowStart computes the header position of one window. A non-empty range
// starts at the first line it contains; an empty range is an insertion point,
// so git records the line after which it happens. When the whole hunk has an
// empty side, its start is already that insertion point.
func windowStart(hunkStart, hunkCount, consumed, windowCount int) int {
	if windowCount > 0 {
		return hunkStart + consumed
	}
	if hunkCount > 0 {
		return hunkStart + consumed - 1
	}
	return hunkStart
}

// groupHunks agrupa ventanas completas en orden, sin partir ninguna. Una
// ventana mayor que maxBytes forma su propio grupo; maxBytes <= 0 desactiva el
// límite.
func groupHunks(hunks []diffHunk, maxBytes int) [][]diffHunk {
	if len(hunks) == 0 {
		return nil
	}
	if maxBytes <= 0 {
		return [][]diffHunk{append([]diffHunk(nil), hunks...)}
	}

	var groups [][]diffHunk
	current := make([]diffHunk, 0, len(hunks))
	currentBytes := 0
	for _, hunk := range hunks {
		size := hunkSize(hunk)
		if len(current) > 0 && currentBytes+size > maxBytes {
			groups = append(groups, current)
			current = make([]diffHunk, 0, len(hunks))
			currentBytes = 0
		}
		current = append(current, hunk)
		currentBytes += size
	}
	if len(current) > 0 {
		groups = append(groups, current)
	}
	return groups
}

// contentLine conserva el texto de un renglón y si lleva salto final.
type contentLine struct {
	text    string
	newline bool
}

func splitContent(data []byte) []contentLine {
	var lines []contentLine
	for len(data) > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			lines = append(lines, contentLine{text: string(data[:i]), newline: true})
			data = data[i+1:]
			continue
		}
		lines = append(lines, contentLine{text: string(data), newline: false})
		break
	}
	return lines
}

func joinContent(lines []contentLine) []byte {
	var b bytes.Buffer
	for _, line := range lines {
		b.WriteString(line.text)
		if line.newline {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// revertHunks reconstruye el contenido anterior al trozo. Aplica de atrás hacia
// adelante: así los hunks posteriores ya revertidos no desplazan las posiciones
// de los anteriores.
func revertHunks(head []byte, hunks []diffHunk) ([]byte, error) {
	lines := splitContent(head)
	for i := len(hunks) - 1; i >= 0; i-- {
		var err error
		lines, err = revertHunk(lines, hunks[i])
		if err != nil {
			return nil, fmt.Errorf("revertir hunk %d: %w", i+1, err)
		}
	}
	return joinContent(lines), nil
}

func revertHunk(lines []contentLine, h diffHunk) ([]contentLine, error) {
	oldLines, newLines, err := hunkSides(h)
	if err != nil {
		return nil, err
	}
	if len(oldLines) != h.OldCount || len(newLines) != h.NewCount {
		return nil, fmt.Errorf("hunk declara %d/%d líneas y su cuerpo tiene %d/%d",
			h.OldCount, h.NewCount, len(oldLines), len(newLines))
	}

	// NewCount == 0 borró líneas en head: P las recupera en la posición
	// 0-based NewStart, es decir, después de los NewStart renglones de head.
	if h.NewCount == 0 {
		if h.NewStart < 0 || h.NewStart > len(lines) {
			return nil, fmt.Errorf("posición %d fuera del archivo (%d líneas)", h.NewStart, len(lines))
		}
		out := make([]contentLine, 0, len(lines)+len(oldLines))
		out = append(out, lines[:h.NewStart]...)
		out = append(out, oldLines...)
		out = append(out, lines[h.NewStart:]...)
		return out, nil
	}

	start := h.NewStart - 1
	if start < 0 || start+h.NewCount > len(lines) {
		return nil, fmt.Errorf("posición %d,%d fuera del archivo (%d líneas)", h.NewStart, h.NewCount, len(lines))
	}
	for i, want := range newLines {
		if lines[start+i] != want {
			return nil, fmt.Errorf("el contenido no coincide en la línea %d", h.NewStart+i)
		}
	}

	out := make([]contentLine, 0, len(lines)-len(newLines)+len(oldLines))
	out = append(out, lines[:start]...)
	out = append(out, oldLines...)
	out = append(out, lines[start+len(newLines):]...)
	return out, nil
}

// hunkSides separa el cuerpo del hunk en sus lados viejo y nuevo. El marcador
// de fin de archivo se asocia al renglón inmediatamente anterior; en un renglón
// de contexto aplica a ambos lados.
func hunkSides(h diffHunk) (oldLines, newLines []contentLine, err error) {
	for i, line := range h.Lines {
		if line == "" {
			return nil, nil, errors.New("línea de hunk vacía")
		}
		if strings.HasPrefix(line, `\`) {
			// Ya se aplicó al renglón anterior mediante la búsqueda hacia delante.
			continue
		}

		newline := true
		if i+1 < len(h.Lines) && strings.HasPrefix(h.Lines[i+1], noNewlineMarker) {
			newline = false
		}
		current := contentLine{text: line[1:], newline: newline}
		switch line[0] {
		case ' ':
			oldLines = append(oldLines, current)
			newLines = append(newLines, current)
		case '-':
			oldLines = append(oldLines, current)
		case '+':
			newLines = append(newLines, current)
		default:
			return nil, nil, fmt.Errorf("prefijo de línea inesperado %q", line[0])
		}
	}
	return oldLines, newLines, nil
}

// chunkParentCommit construye P y C de modo que `git diff P C` sea exactamente
// el trozo. Para un archivo que existe en head, C es el árbol de head y P es
// head con el trozo revertido. Para un archivo eliminado, P conserva la base
// completa y C aplica solo el trozo: así el lado viejo mantiene su numeración y
// el SHA de referencia es el de la base.
func chunkParentCommit(ctx context.Context, dir, baseSHA, headSHA, path string, chunk []diffHunk) (parentSHA, chunkSHA string, err error) {
	if len(chunk) == 0 {
		return "", "", errors.New("chunk de diff vacío")
	}

	headContent, headMode, exists, err := blobAt(ctx, dir, headSHA, path)
	if err != nil {
		return "", "", err
	}
	if !exists {
		baseContent, baseMode, found, err := blobAt(ctx, dir, baseSHA, path)
		if err != nil {
			return "", "", err
		}
		if !found {
			return "", "", fmt.Errorf("chunk: %s no existe ni en base ni en head", path)
		}
		chunkContent, err := applyHunks(baseContent, chunk)
		if err != nil {
			return "", "", fmt.Errorf("aplicar chunk de %s: %w", path, err)
		}
		parentTree, err := treeWithBlob(ctx, dir, headSHA+"^{tree}", path, baseMode, baseContent)
		if err != nil {
			return "", "", err
		}
		// A window that removes the whole remaining content leaves the path
		// absent in C, exactly as head records it; adding an empty blob would
		// turn a deletion into an empty file.
		chunkTree := headSHA + "^{tree}"
		if len(chunkContent) > 0 {
			chunkTree, err = treeWithBlob(ctx, dir, headSHA+"^{tree}", path, baseMode, chunkContent)
			if err != nil {
				return "", "", err
			}
		}
		return commitTreePair(ctx, dir, baseSHA, parentTree, chunkTree)
	}

	parentContent, err := revertHunks(headContent, chunk)
	if err != nil {
		return "", "", fmt.Errorf("revertir chunk de %s: %w", path, err)
	}
	parentTree, err := treeWithBlob(ctx, dir, headSHA+"^{tree}", path, headMode, parentContent)
	if err != nil {
		return "", "", err
	}
	return commitTreePair(ctx, dir, baseSHA, parentTree, headSHA+"^{tree}")
}

// blobAt returns the content and mode of path at ref. A path absent from that
// revision is reported with exists=false instead of an error.
func blobAt(ctx context.Context, dir, ref, path string) (content []byte, mode string, exists bool, err error) {
	mode, exists, err = fileModeAt(ctx, dir, ref, path)
	if err != nil || !exists {
		return nil, "", exists, err
	}
	content, err = gitRaw(ctx, dir, "show blob", "show", ref+":"+path)
	if err != nil {
		return nil, "", false, err
	}
	return content, mode, true, nil
}

// fileModeAt returns the mode with which ref records path (100644, 100755,
// 120000, 160000, ...) and whether the path exists at all.
func fileModeAt(ctx context.Context, dir, ref, path string) (string, bool, error) {
	out, err := gitRaw(ctx, dir, "ls-tree", "ls-tree", ref, "--", path)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 || fields[1] != "blob" {
		return "", false, nil
	}
	return fields[0], true, nil
}

// treeWithBlob rewrites baseTree with path pointing at content. It keeps the
// head tree for every other path, so P..C only differs at path.
func treeWithBlob(ctx context.Context, dir, baseTree, path, mode string, content []byte) (string, error) {
	blobRaw, err := gitRawEnv(ctx, dir, "hash-object", nil, content,
		"hash-object", "-w", "--path="+path, "--stdin")
	if err != nil {
		return "", err
	}
	blob := strings.TrimSpace(string(blobRaw))
	if blob == "" {
		return "", errors.New("git hash-object: blob vacío")
	}

	tmpDir, err := os.MkdirTemp("", "qa-diffchunk-")
	if err != nil {
		return "", fmt.Errorf("crear índice temporal: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(tmpDir, "index")}
	if _, err := gitRawEnv(ctx, dir, "read-tree", indexEnv, nil, "read-tree", baseTree); err != nil {
		return "", err
	}
	// La forma separada evita ambigüedades con paths que contienen comas.
	if _, err := gitRawEnv(ctx, dir, "update-index", indexEnv, nil,
		"update-index", "--add", "--cacheinfo", mode, blob, path); err != nil {
		return "", err
	}
	treeRaw, err := gitRawEnv(ctx, dir, "write-tree", indexEnv, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(treeRaw)), nil
}

// commitTreePair commits the synthetic pair: P hangs from the reviewed base and
// C hangs from P, so `git diff P C` is the isolated chunk.
func commitTreePair(ctx context.Context, dir, baseSHA, parentTree, chunkTree string) (parentSHA, chunkSHA string, err error) {
	identity := syntheticIdentityEnv()
	parentRaw, err := gitRawEnv(ctx, dir, "commit-tree parent", identity, nil,
		"commit-tree", parentTree, "-p", baseSHA)
	if err != nil {
		return "", "", err
	}
	parentSHA = strings.TrimSpace(string(parentRaw))

	chunkRaw, err := gitRawEnv(ctx, dir, "commit-tree chunk", identity, nil,
		"commit-tree", chunkTree, "-p", parentSHA)
	if err != nil {
		return "", "", err
	}
	chunkSHA = strings.TrimSpace(string(chunkRaw))
	return parentSHA, chunkSHA, nil
}

// applyHunks reconstructs the result of applying hunks to base content. It
// walks backwards so earlier positions stay valid while later hunks change the
// line count.
func applyHunks(base []byte, hunks []diffHunk) ([]byte, error) {
	lines := splitContent(base)
	for i := len(hunks) - 1; i >= 0; i-- {
		h := hunks[i]
		oldLines, newLines, err := hunkSides(h)
		if err != nil {
			return nil, err
		}
		if len(oldLines) != h.OldCount || len(newLines) != h.NewCount {
			return nil, fmt.Errorf("hunk declara %d/%d líneas y su cuerpo tiene %d/%d",
				h.OldCount, h.NewCount, len(oldLines), len(newLines))
		}
		start := h.OldStart
		if h.OldCount > 0 {
			start = h.OldStart - 1
		}
		if start < 0 || start+len(oldLines) > len(lines) {
			return nil, fmt.Errorf("posición %d,%d fuera del archivo (%d líneas)",
				h.OldStart, h.OldCount, len(lines))
		}
		for j, want := range oldLines {
			if lines[start+j] != want {
				return nil, fmt.Errorf("el contenido no coincide en la línea %d", h.OldStart+j)
			}
		}
		out := make([]contentLine, 0, len(lines)-len(oldLines)+len(newLines))
		out = append(out, lines[:start]...)
		out = append(out, newLines...)
		out = append(out, lines[start+len(oldLines):]...)
		lines = out
	}
	return joinContent(lines), nil
}

// syntheticIdentityEnv fija la identidad de los commits sintéticos: un clon
// shallow puede no tener gitconfig con user.name/user.email.
func syntheticIdentityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=qa-review",
		"GIT_AUTHOR_EMAIL=qa-review@localhost",
		"GIT_COMMITTER_NAME=qa-review",
		"GIT_COMMITTER_EMAIL=qa-review@localhost",
		"GIT_AUTHOR_DATE=2000-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2000-01-01T00:00:00Z",
	}
}

// chunkFileDiff parte el diff de un path en ventanas y grupos de hunks y
// devuelve un par P/C por grupo. Un archivo eliminado en head también se
// trocea: sus líneas se revisan desde la base. Un diff con rename se rechaza y
// devuelve nil porque `--no-renames` no debería producirlo.
func chunkFileDiff(ctx context.Context, dir, baseSHA, headSHA, path string, maxBytes int) ([]chunk, error) {
	raw, err := gitRaw(ctx, dir, "diff", "diff", "-U0", "--no-color", "--no-renames",
		baseSHA, headSHA, "--", path)
	if err != nil {
		return nil, err
	}

	hunks, err := parseFileDiff(raw)
	if errors.Is(err, errRenameDiff) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	groups := groupHunks(windowHunks(hunks, maxBytes), maxBytes)
	if len(groups) == 0 {
		return nil, nil
	}
	chunks := make([]chunk, 0, len(groups))
	for _, group := range groups {
		parent, commit, err := chunkParentCommit(ctx, dir, baseSHA, headSHA, path, group)
		if err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk{Parent: parent, Commit: commit, Hunks: group})
	}
	return chunks, nil
}
