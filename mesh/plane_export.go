package mesh

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type planeExportWriter struct {
	ctx        context.Context
	w          io.Writer
	bytes, max int64
}

func (w *planeExportWriter) Write(p []byte) (int, error) {
	if e := w.ctx.Err(); e != nil {
		return 0, e
	}
	if int64(len(p)) > w.max-w.bytes {
		return 0, &QualityLimitError{"plane export bytes", w.bytes + int64(len(p)), w.max}
	}
	n, e := w.w.Write(p)
	w.bytes += int64(n)
	return n, e
}

// WritePLYFile stages a closed clipping result in the destination directory.
// On error/cancel the temporary is removed and the old destination remains.
// Successful Rename is the commit point: cancellation after it cannot turn a
// published result into a failed return. Zero maxBytes selects 512 MiB. This
// is local file publication, not a cross-platform crash-durability guarantee.
// Callers must not mutate r during export. Source identity, normalized plane,
// retained side and original/cap face colors are recorded as PLY comments and
// properties; SourceTriangles remains the full in-memory face correspondence.
func (r *PlaneClipResult) WritePLYFile(ctx context.Context, path string, maxBytes int64) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if r == nil || r.Mesh == nil || path == "" || maxBytes < 0 {
		return fmt.Errorf("%w: export request", ErrPlaneGeometry)
	}
	if maxBytes == 0 {
		maxBytes = 512 << 20
	}
	if e := r.Mesh.Validate(); e != nil {
		return e
	}
	if !r.Mesh.CheckClosure().Closed() {
		return fmt.Errorf("%w: export requires closed result", ErrPlaneGeometry)
	}
	temp, e := os.CreateTemp(filepath.Dir(path), ".mesh-plane-*.ply")
	if e != nil {
		return e
	}
	name := temp.Name()
	defer func() { _ = temp.Close(); _ = os.Remove(name) }()
	comments := []string{"derived source " + r.SourceReference, fmt.Sprintf("plane origin %v normal %v keep_positive %v", r.Plane.Origin, r.Plane.Normal, r.KeepPositive), "source triangle correspondence is provided in PlaneClipResult.SourceTriangles; cap=-1"}
	w := &planeExportWriter{ctx: ctx, w: temp, max: maxBytes}
	if e = WritePLY(w, r.Mesh, PLYOptions{VertexScalars: r.VertexScalars, FaceColors: r.FaceColors, Comments: comments}); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = temp.Sync(); e != nil {
		return e
	}
	if e = temp.Close(); e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	return os.Rename(name, path)
}
