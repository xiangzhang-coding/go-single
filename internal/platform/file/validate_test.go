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
			ext, mime, err := validate(tc.header, 1024)
			require.NoError(t, err)
			require.Equal(t, tc.wantExt, ext)
			require.Equal(t, tc.wantMIME, mime)
		})
	}
}

func TestValidateRejectsInvalidType(t *testing.T) {
	// 文本内容（即使伪造扩展名）拒绝。
	_, _, err := validate(txtHeader, 1024)
	require.ErrorIs(t, err, ErrInvalidType)

	// 空文件拒绝。
	_, _, err = validate(nil, 0)
	require.ErrorIs(t, err, ErrInvalidType)
}

func TestValidateRejectsTooLarge(t *testing.T) {
	// 恰好上限通过，超过上限拒绝（无论类型是否合法）。
	_, _, err := validate(pngHeader, MaxFileSize)
	require.NoError(t, err)

	_, _, err = validate(pngHeader, MaxFileSize+1)
	require.ErrorIs(t, err, ErrTooLarge)

	_, _, err = validate(txtHeader, MaxFileSize+1)
	require.ErrorIs(t, err, ErrTooLarge)
}
