package archive

import (
	"context"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
)

func BuildMediaPreviews(ctx context.Context, original, mime, root, sha string) (string, string, error) {
	if !strings.HasPrefix(strings.ToLower(mime), "image/") {
		return "", "", nil
	}
	f, err := os.Open(original)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, "thumbnails", sha[:2], sha[2:4])
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", "", err
	}
	thumb := filepath.Join(dir, sha+"-320.jpg")
	preview := filepath.Join(dir, sha+"-1280.jpg")
	for _, x := range []struct {
		p   string
		max int
	}{{thumb, 320}, {preview, 1280}} {
		if _, e := os.Stat(x.p); e == nil {
			continue
		}
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		scale := float64(x.max) / float64(max(w, h))
		if scale > 1 {
			scale = 1
		}
		nw, nh := int(float64(w)*scale), int(float64(h)*scale)
		dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
		// ApproxBiLinear is substantially cheaper than CatmullRom and is more than
		// sufficient for archive thumbnails/previews. Preview generation must not
		// compete with the browser for CPU while a download is in progress.
		draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, b, draw.Over, nil)
		out, e := os.Create(x.p)
		if e != nil {
			return "", "", e
		}
		e = jpeg.Encode(out, dst, &jpeg.Options{Quality: 88})
		_ = out.Close()
		if e != nil {
			return "", "", e
		}
	}
	return thumb, preview, nil
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
