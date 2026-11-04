package thumb

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestGenerateThumbnailAndCache(t *testing.T) {
	// 构造一张 1200x800 的较大 PNG 原图
	src := image.NewRGBA(image.Rect(0, 0, 1200, 800))
	for y := 0; y < 800; y++ {
		for x := 0; x < 1200; x++ {
			src.SetRGBA(x, y, color.RGBA{R: uint8(x % 255), G: uint8(y % 255), B: 180, A: 255})
		}
	}
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, src); err != nil {
		t.Fatal(err)
	}

	// 按 200x200 生成缩略图 → 期望等比缩小为 200x133
	out, err := Generate(bytes.NewReader(pngBuf.Bytes()), Options{MaxWidth: 200, MaxHeight: 200, Quality: 75})
	if err != nil {
		t.Fatalf("生成缩略图失败: %v", err)
	}
	cfg, fmtName, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码缩略图失败: %v", err)
	}
	if fmtName != "jpeg" {
		t.Fatalf("期望输出 jpeg，实际 %s", fmtName)
	}
	if cfg.Width != 200 || cfg.Height != 133 {
		t.Fatalf("期望等比缩放尺寸 200x133，实际 %dx%d", cfg.Width, cfg.Height)
	}
	if len(out) >= pngBuf.Len()/2 {
		t.Fatalf("缩略图体积应显著小于原图: 原图=%d, 缩略图=%d", pngBuf.Len(), len(out))
	}

	// 验证 LRU 缓存
	c := NewCache(2)
	k1, etag1 := MakeKey("s1", "a.png", 100, 1024, 200, 200, 75)
	c.Put(k1, etag1, out)
	got, gotEtag, ok := c.Get(k1)
	if !ok || gotEtag != etag1 || len(got) != len(out) {
		t.Fatal("缓存读取不匹配")
	}
}
