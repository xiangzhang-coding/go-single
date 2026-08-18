package file

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 白名单各类型的魔数头部（覆盖 mimetype 检测所需的签名）。
var (
	pngHeader  = []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52}
	jpegHeader = []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01}
	gifHeader  = []byte("GIF89a\x01\x00\x01\x00")
	webpHeader = []byte{'R', 'I', 'F', 'F', 0x00, 0x00, 0x00, 0x00, 'W', 'E', 'B', 'P', 'V', 'P', '8', ' '}
	txtHeader  = []byte("hello world, plain text content")
)

func TestValidateAllowedTypes(t *testing.T) {
	cases := []struct {
		name     string
		header   []byte
		wantExt  string
		wantMIME string
	}{
		{"png", pngHeader, "png", "image/png"},
		{"jpeg", jpegHeader, "jpg", "image/jpeg"},
		{"gif", gifHeader, "gif", "image/gif"},
		{"webp", webpHeader, "webp", "image/webp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, mime, err := validateUpload(KindImage, tc.header, 1024, "image.bin")
			require.NoError(t, err)
			require.Equal(t, tc.wantExt, ext)
			require.Equal(t, tc.wantMIME, mime)
		})
	}
}

func TestValidateRejectsInvalidType(t *testing.T) {
	// 文本内容（即使伪造扩展名）拒绝。
	_, _, err := validateUpload(KindImage, txtHeader, 1024, "avatar.png")
	require.ErrorIs(t, err, ErrInvalidType)

	// 空文件拒绝。
	_, _, err = validateUpload(KindImage, nil, 0, "empty.png")
	require.ErrorIs(t, err, ErrInvalidType)
}

func TestValidateRejectsTooLarge(t *testing.T) {
	// 恰好上限通过，超过上限拒绝（无论类型是否合法）。
	_, _, err := validateUpload(KindImage, pngHeader, MaxImageSize, "avatar.png")
	require.NoError(t, err)

	_, _, err = validateUpload(KindImage, pngHeader, MaxImageSize+1, "avatar.png")
	require.ErrorIs(t, err, ErrTooLarge)

	_, _, err = validateUpload(KindImage, txtHeader, MaxImageSize+1, "avatar.png")
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestValidateFileMessagePolicy(t *testing.T) {
	cases := []struct {
		name     string
		filename string
		header   []byte
		wantExt  string
		wantMIME string
	}{
		{name: "pdf", filename: "manual.pdf", header: []byte("%PDF-1.7\n"), wantExt: "pdf", wantMIME: "application/pdf"},
		{name: "zip", filename: "archive.zip", header: []byte{'P', 'K', 0x03, 0x04, 0x14, 0x00}, wantExt: "zip", wantMIME: "application/zip"},
		{name: "text", filename: "notes.txt", header: txtHeader, wantExt: "txt", wantMIME: "text/plain; charset=utf-8"},
		{name: "csv", filename: "items.csv", header: []byte("sku,quantity\n1,2\n"), wantExt: "csv", wantMIME: "text/csv; charset=utf-8"},
		{name: "markdown", filename: "readme.md", header: []byte("# heading\nbody\n"), wantExt: "md", wantMIME: "text/markdown; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ext, mime, err := validateUpload(KindFile, tc.header, int64(len(tc.header)), tc.filename)
			require.NoError(t, err)
			require.Equal(t, tc.wantExt, ext)
			require.Equal(t, tc.wantMIME, mime)
		})
	}

	_, _, err := validateUpload(KindFile, []byte("<script>alert(1)</script>"), 25, "attack.html")
	require.ErrorIs(t, err, ErrInvalidType)
	_, _, err = validateUpload(KindFile, pngHeader, int64(len(pngHeader)), "renamed.pdf")
	require.ErrorIs(t, err, ErrInvalidType)
	_, _, err = validateUpload(KindFile, []byte("%PDF-1.7\n"), 9, "renamed.txt")
	require.ErrorIs(t, err, ErrInvalidType)
	_, _, err = validateUpload(KindFile, txtHeader, MaxMessageFileSize+1, "notes.txt")
	require.ErrorIs(t, err, ErrTooLarge)
}

func TestValidateRejectsUnknownKindAndKindMismatch(t *testing.T) {
	_, _, err := validateUpload("video", pngHeader, int64(len(pngHeader)), "clip.png")
	require.ErrorIs(t, err, ErrInvalidKind)

	_, _, err = validateUpload(KindFile, pngHeader, int64(len(pngHeader)), "image.png")
	require.ErrorIs(t, err, ErrInvalidType)
	_, _, err = validateUpload(KindImage, []byte("%PDF-1.7\n"), 9, "manual.pdf")
	require.ErrorIs(t, err, ErrInvalidType)
}
